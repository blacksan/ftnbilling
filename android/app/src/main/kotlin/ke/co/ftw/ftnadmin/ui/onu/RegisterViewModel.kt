package ke.co.ftw.ftnadmin.ui.onu

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import ke.co.ftw.ftnadmin.core.network.ApiError
import ke.co.ftw.ftnadmin.data.equipment.EquipmentRepository
import ke.co.ftw.ftnadmin.data.equipment.Olt
import ke.co.ftw.ftnadmin.data.onu.OnuRepository
import ke.co.ftw.ftnadmin.data.onu.RegisterItem
import ke.co.ftw.ftnadmin.domain.MacAddress
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.common.apiResult
import ke.co.ftw.ftnadmin.ui.common.userMessage
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch

data class AddOnuForm(
    val serial: String = "",
    val gponSerial: String = "",
    val mac: String = "",
    val model: String = "",
    /** The OLT it hangs off and the PON port (as typed); both optional. */
    val oltId: String = "",
    val ponPort: String = "",
    val errors: Map<String, String> = emptyMap(),
)

data class RegisterUiState(
    val items: Loadable<List<RegisterItem>> = Loadable.Loading,
    /** The OLTs of the register, for the placement chips of the add sheet. */
    val olts: List<Olt> = emptyList(),
    val adding: Boolean = false,
    val form: AddOnuForm = AddOnuForm(),
    val busy: Boolean = false,
    val error: String? = null,
    val info: String? = null,
    /** Pull to refresh is running (the list stays on screen). */
    val refreshing: Boolean = false,
)

/** The ONU register: the serial + WAN MAC pairs of the ONUs the owner supplies. */
class RegisterViewModel(
    private val repo: OnuRepository,
    private val equipment: EquipmentRepository? = null,
) : ViewModel() {
    private val _state = MutableStateFlow(RegisterUiState())
    val state: StateFlow<RegisterUiState> = _state.asStateFlow()

    init {
        load()
    }

    /** Pull to refresh: reloads without replacing the list by a spinner. */
    fun refresh() {
        _state.update { it.copy(refreshing = true) }
        load()
    }

    fun load() {
        viewModelScope.launch {
            val r = apiResult { repo.register() }
            _state.update { cur ->
                r.fold(
                    { l -> cur.copy(items = Loadable.Ready(l), refreshing = false) },
                    { e -> cur.copy(items = Loadable.Failed(e.userMessage()), refreshing = false) },
                )
            }
        }
    }

    private fun loadOlts() {
        val eq = equipment ?: return
        viewModelScope.launch { apiResult { eq.olts() }.getOrNull()?.let { l -> _state.update { it.copy(olts = l) } } }
    }

    fun clearInfo() = _state.update { it.copy(info = null, error = null) }

    fun openAdd() {
        _state.update { it.copy(adding = true, form = AddOnuForm(), error = null) }
        loadOlts()
    }

    /** Picks the OLT (a second tap on the same one clears it). */
    fun onOlt(id: String) = edit { copy(oltId = if (oltId == id) "" else id, errors = errors - "pon_port") }

    fun onPonPort(v: String) = edit { copy(ponPort = v.filter { it.isDigit() }.take(3), errors = errors - "pon_port") }

    fun closeAdd() = _state.update { it.copy(adding = false, form = AddOnuForm(), error = null) }

    fun onSerial(v: String) = edit { copy(serial = v, errors = errors - "serial") }

    fun onGpon(v: String) = edit { copy(gponSerial = v) }

    fun onMac(v: String) = edit { copy(mac = v, errors = errors - "mac") }

    fun onModel(v: String) = edit { copy(model = v) }

    private fun edit(f: AddOnuForm.() -> AddOnuForm) = _state.update { it.copy(form = it.form.f(), error = null) }

    /** Field problems of [f]; empty when it can be sent. */
    fun validate(f: AddOnuForm): Map<String, String> = buildMap {
        if (f.serial.isBlank()) put("serial", "Enter the serial number.")
        if (MacAddress.normalize(f.mac) == null) put("mac", MacAddress.errorFor(f.mac) ?: "Enter the WAN MAC the router sees.")
        if (f.oltId.isNotBlank() && (f.ponPort.toIntOrNull() ?: 0) <= 0) put("pon_port", "Enter the PON port of the OLT.")
    }

    fun save() {
        val f = _state.value.form
        val problems = validate(f)
        if (problems.isNotEmpty()) {
            _state.update { it.copy(form = it.form.copy(errors = problems)) }
            return
        }
        val mac = MacAddress.normalize(f.mac) ?: return
        _state.update { it.copy(busy = true, error = null) }
        viewModelScope.launch {
            val r = apiResult { repo.addRegisterItem(f.serial, f.gponSerial, mac, f.model, f.oltId.ifBlank { null }, f.ponPort.toIntOrNull() ?: 0) }
            r.fold(
                {
                    _state.update { it.copy(busy = false, adding = false, form = AddOnuForm(), info = "ONU added to the register") }
                    load()
                },
                { e ->
                    val api = e as? ApiError
                    _state.update { cur ->
                        val fe = api?.fieldErrors.orEmpty()
                        if (fe.isNotEmpty()) cur.copy(busy = false, form = cur.form.copy(errors = cur.form.errors + fe))
                        else cur.copy(busy = false, error = e.userMessage())
                    }
                },
            )
        }
    }
}
