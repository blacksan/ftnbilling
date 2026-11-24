package ke.co.ftw.ftnadmin.ui.equipment

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import ke.co.ftw.ftnadmin.data.equipment.EquipmentRepository
import ke.co.ftw.ftnadmin.data.equipment.EquipmentTree
import ke.co.ftw.ftnadmin.data.equipment.OltStatus
import ke.co.ftw.ftnadmin.data.equipment.TreeOlt
import ke.co.ftw.ftnadmin.data.equipment.TreeOnu
import ke.co.ftw.ftnadmin.data.equipment.TreePon
import ke.co.ftw.ftnadmin.data.equipment.TreeRouter
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.common.apiResult
import ke.co.ftw.ftnadmin.ui.common.userMessage
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch

/** Id of the pseudo router that holds what is not placed yet (OLTs without a router, ONUs without an OLT). */
const val UNPLACED = "unplaced"

/** Where the drill-down is: nothing chosen, a router, an OLT of it, a PON port of that OLT. */
enum class EquipmentLevel { Root, Router, Olt, Pon }

/** The sheet that puts an ONU under an OLT's PON port (or takes it out). */
data class MoveSheet(
    val onu: TreeOnu,
    val oltId: String = "",
    /** Typed port number; blank is "no port yet". */
    val port: String = "",
    val busy: Boolean = false,
    val error: String? = null,
)

data class EquipmentUiState(
    val tree: Loadable<EquipmentTree> = Loadable.Loading,
    val routerId: String? = null,
    val oltId: String? = null,
    val ponPort: Int? = null,
    val move: MoveSheet? = null,
    /** Read-only OLT status of the open OLT; null until it is read, or when the server has no OLT management. */
    val oltStatus: OltStatus? = null,
    val info: String? = null,
) {
    private val data: EquipmentTree? get() = (tree as? Loadable.Ready)?.value

    val level: EquipmentLevel
        get() = when {
            routerId == null -> EquipmentLevel.Root
            oltId == null -> EquipmentLevel.Router
            ponPort == null -> EquipmentLevel.Olt
            else -> EquipmentLevel.Pon
        }

    val router: TreeRouter? get() = data?.routers?.firstOrNull { it.id == routerId }

    /** OLTs shown under the chosen router; the pseudo router [UNPLACED] lists the OLTs with no router. */
    val olts: List<TreeOlt> get() = if (routerId == UNPLACED) data?.unplacedOlts.orEmpty() else router?.olts.orEmpty()

    val olt: TreeOlt? get() = olts.firstOrNull { it.id == oltId }

    val pon: TreePon? get() = olt?.pons?.firstOrNull { it.port == ponPort }

    /** Every OLT of the tree, for the "move ONU" picker. */
    val allOlts: List<TreeOlt> get() = data?.routers.orEmpty().flatMap { it.olts } + data?.unplacedOlts.orEmpty()

    val unplacedOnus: List<TreeOnu> get() = data?.unplacedOnus.orEmpty()

    /** The title of the screen at this level. */
    val title: String
        get() = when (level) {
            EquipmentLevel.Root -> "Equipment tree"
            EquipmentLevel.Router -> if (routerId == UNPLACED) "Not placed" else router?.name ?: "Router"
            EquipmentLevel.Olt -> olt?.name ?: "OLT"
            EquipmentLevel.Pon -> "PON port $ponPort"
        }
}

/** Equipment: router, then OLT, then PON port, then ONUs, each level a screen of its own in one back stack. */
class EquipmentViewModel(private val repo: EquipmentRepository) : ViewModel() {
    private val _state = MutableStateFlow(EquipmentUiState())
    val state: StateFlow<EquipmentUiState> = _state.asStateFlow()

    init {
        load()
    }

    fun load() {
        viewModelScope.launch {
            val r = apiResult { repo.tree() }
            _state.update { cur ->
                r.fold(
                    { t -> cur.copy(tree = Loadable.Ready(t)) },
                    { e -> cur.copy(tree = Loadable.Failed(e.userMessage())) },
                )
            }
        }
    }

    fun clearInfo() = _state.update { it.copy(info = null) }

    fun openRouter(id: String) = _state.update { it.copy(routerId = id, oltId = null, ponPort = null) }

    fun openOlt(id: String) {
        _state.update { it.copy(oltId = id, ponPort = null, oltStatus = null) }
        viewModelScope.launch {
            apiResult { repo.oltStatus(id) }.onSuccess { st ->
                _state.update { cur -> if (cur.oltId == id) cur.copy(oltStatus = st) else cur }
            }
        }
    }

    fun openPon(port: Int) = _state.update { it.copy(ponPort = port) }

    /** One level up. False at the root: the screen then leaves. */
    fun up(): Boolean {
        val s = _state.value
        return when (s.level) {
            EquipmentLevel.Root -> false
            EquipmentLevel.Router -> { _state.update { it.copy(routerId = null) }; true }
            EquipmentLevel.Olt -> { _state.update { it.copy(oltId = null) }; true }
            EquipmentLevel.Pon -> { _state.update { it.copy(ponPort = null) }; true }
        }
    }

    // ---- placing an ONU ----

    fun startMove(onu: TreeOnu) =
        _state.update { it.copy(move = MoveSheet(onu, oltId = onu.oltId, port = if (onu.ponPort > 0) onu.ponPort.toString() else "")) }

    fun cancelMove() = _state.update { it.copy(move = null) }

    fun setMoveOlt(id: String) = editMove { copy(oltId = if (oltId == id) "" else id, error = null) }

    fun setMovePort(v: String) = editMove { copy(port = v.filter { it.isDigit() }.take(3), error = null) }

    private fun editMove(f: MoveSheet.() -> MoveSheet) = _state.update { s -> s.move?.let { s.copy(move = it.f()) } ?: s }

    /** The port the sheet will send: blank is 0 (no port); null when it is not a number. */
    fun movePortValue(m: MoveSheet): Int? = if (m.port.isBlank()) 0 else m.port.toIntOrNull()

    fun applyMove() {
        val m = _state.value.move ?: return
        val port = movePortValue(m) ?: return
        if (m.oltId.isNotBlank() && port <= 0) {
            editMove { copy(error = "Choose the PON port the ONU is on.") }
            return
        }
        editMove { copy(busy = true, error = null) }
        viewModelScope.launch {
            val r = apiResult { repo.setPlacement(m.onu.id, if (m.oltId.isBlank()) "" else m.oltId, if (m.oltId.isBlank()) 0 else port) }
            r.fold(
                { p ->
                    // A house the PON port does not serve is a warning, never a refusal: say it, but the change is made.
                    val note = p.warnings.firstOrNull()
                    _state.update { it.copy(move = null, info = note ?: if (p.oltId.isBlank()) "ONU taken out of its OLT" else "ONU placed on ${p.oltName}, PON ${p.ponPort}") }
                    load()
                },
                { e -> editMove { copy(busy = false, error = (e as? ke.co.ftw.ftnadmin.core.network.ApiError)?.fieldErrors?.values?.firstOrNull() ?: e.userMessage()) } },
            )
        }
    }
}
