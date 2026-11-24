package ke.co.ftw.ftnadmin.ui.equipment

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import ke.co.ftw.ftnadmin.data.equipment.EquipmentRepository
import ke.co.ftw.ftnadmin.data.equipment.EquipmentTree
import ke.co.ftw.ftnadmin.data.equipment.TreeOlt
import ke.co.ftw.ftnadmin.data.equipment.TreeOnu
import ke.co.ftw.ftnadmin.data.onu.RegisterItem
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.common.apiResult
import ke.co.ftw.ftnadmin.ui.common.userMessage
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch

/** The four tabs of Equipment, in display order. [key] is what [ke.co.ftw.ftnadmin.ui.nav.EquipmentRoute] carries. */
enum class EquipmentTab(val key: String, val label: String) {
    Routers("routers", "Routers"),
    Olt("olt", "OLT"),
    Onu("onu", "ONU"),
    Other("other", "Other"),
    ;

    companion object {
        /** The tab for a route argument; anything unknown (or null) is Routers, the default. */
        fun fromKey(key: String?): EquipmentTab = entries.firstOrNull { it.key == key } ?: Routers
    }
}

/** The register kinds that have a tab of their own. Everything else is "Other". */
const val KIND_OLT = "olt"
const val KIND_ONT = "ont"

fun isOtherKind(kind: String): Boolean = kind != KIND_OLT && kind != KIND_ONT

/** What the Other tab lists: every register item that is not an OLT or an ONU. */
fun otherItems(all: List<RegisterItem>): List<RegisterItem> = all.filter { isOtherKind(it.kind) }

/** The counts shown on the tabs; null while a list has not loaded (the tab shows no number yet). */
data class EquipmentTabCounts(val routers: Int? = null, val olts: Int? = null, val onus: Int? = null, val other: Int? = null) {
    fun of(tab: EquipmentTab): Int? = when (tab) {
        EquipmentTab.Routers -> routers
        EquipmentTab.Olt -> olts
        EquipmentTab.Onu -> onus
        EquipmentTab.Other -> other
    }
}

/** The tab label with its count: "ONU (12)", or just "ONU" before the list is there. */
fun tabTitle(tab: EquipmentTab, counts: EquipmentTabCounts): String = counts.of(tab)?.let { "${tab.label} ($it)" } ?: tab.label

/** One OLT row: the OLT and the router it sits on ("" when it is not placed). */
data class OltRow(val olt: TreeOlt, val routerName: String)

/** One ONU row: the ONU, its router and OLT names ("" when not placed). */
data class OnuRow(val onu: TreeOnu, val routerId: String, val routerName: String, val oltName: String)

fun EquipmentTree.oltRows(): List<OltRow> =
    routers.flatMap { r -> r.olts.map { OltRow(it, r.name) } } + unplacedOlts.map { OltRow(it, "") }

fun EquipmentTree.onuRows(): List<OnuRow> {
    val placed = routers.flatMap { r -> r.olts.flatMap { o -> o.pons.flatMap { p -> p.onus.map { OnuRow(it, r.id, r.name, o.name) } } } }
    val loose = unplacedOlts.flatMap { o -> o.pons.flatMap { p -> p.onus.map { OnuRow(it, "", "", o.name) } } }
    return placed + loose + unplacedOnus.map { OnuRow(it, "", "", "") }
}

/** Register statuses an ONU filter offers, with the words the owner reads ("installed" reads as "deployed"). */
val ONU_STATUSES: List<String> = listOf("in_stock", "installed", "faulty", "returned")

fun onuStatusLabel(status: String): String = when (status) {
    "in_stock" -> "In stock"
    "installed" -> "Deployed"
    "faulty" -> "Faulty"
    "returned" -> "Returned"
    "" -> ""
    else -> status.replace('_', ' ').replaceFirstChar { it.uppercase() }
}

/** The ONU list under the filters: [query] matches serial, GPON serial, MAC, model, house and customer; blank filters match all. */
fun filterOnuRows(rows: List<OnuRow>, query: String, status: String, oltId: String): List<OnuRow> {
    val q = query.trim().lowercase()
    val qMac = q.replace(":", "")
    return rows.filter { r ->
        val o = r.onu
        (status.isBlank() || o.status == status) &&
            (oltId.isBlank() || o.oltId == oltId) &&
            (
                q.isEmpty() ||
                    listOf(o.serial, o.gponSerial, o.model, o.house?.unit.orEmpty(), o.house?.customer.orEmpty(), o.house?.apartment.orEmpty())
                        .any { it.lowercase().contains(q) } ||
                    (qMac.isNotEmpty() && o.mac.lowercase().replace(":", "").contains(qMac))
                )
    }
}

/** The Other list under the search box and status filter. */
fun filterOther(items: List<RegisterItem>, query: String): List<RegisterItem> {
    val q = query.trim().lowercase()
    if (q.isEmpty()) return items
    val qMac = q.replace(":", "")
    return items.filter { i ->
        listOf(i.kind, i.serial, i.model).any { it.lowercase().contains(q) } ||
            (qMac.isNotEmpty() && i.mac.lowercase().replace(":", "").contains(qMac))
    }
}

/** The tab counts from what has loaded: routers (the router list), the tree (OLTs and ONUs) and the register (Other). */
fun equipmentTabCounts(routers: Int?, tree: EquipmentTree?, other: List<RegisterItem>?): EquipmentTabCounts =
    EquipmentTabCounts(
        routers = routers,
        olts = tree?.oltRows()?.size,
        onus = tree?.onuRows()?.size,
        other = other?.let { otherItems(it).size },
    )

data class EquipmentTabsUiState(
    val tab: EquipmentTab = EquipmentTab.Routers,
    /** The register, for the Other tab (the Routers, OLT and ONU tabs read the router list and the tree). */
    val register: Loadable<List<RegisterItem>> = Loadable.Loading,
    val onuQuery: String = "",
    /** "" is every status. */
    val onuStatus: String = "",
    /** "" is every OLT. */
    val onuOltId: String = "",
    val otherQuery: String = "",
) {
    val other: Loadable<List<RegisterItem>>
        get() = when (val r = register) {
            is Loadable.Ready -> Loadable.Ready(otherItems(r.value))
            else -> r
        }
}

/**
 * The tab state of Equipment: which tab is open, the filters of the ONU and Other lists, and the register for Other.
 * The Routers tab reuses RoutersViewModel and the OLT and ONU tabs reuse EquipmentViewModel's tree; this view model
 * only adds what those do not hold.
 */
class EquipmentTabsViewModel(private val repo: EquipmentRepository, initial: EquipmentTab = EquipmentTab.Routers) : ViewModel() {
    private val _state = MutableStateFlow(EquipmentTabsUiState(tab = initial))
    val state: StateFlow<EquipmentTabsUiState> = _state.asStateFlow()

    init {
        loadRegister()
    }

    fun loadRegister() {
        viewModelScope.launch {
            val r = apiResult { repo.items() }
            _state.update { cur ->
                r.fold(
                    { l -> cur.copy(register = Loadable.Ready(l)) },
                    { e -> cur.copy(register = Loadable.Failed(e.userMessage())) },
                )
            }
        }
    }

    fun selectTab(t: EquipmentTab) = _state.update { it.copy(tab = t) }

    fun setOnuQuery(v: String) = _state.update { it.copy(onuQuery = v) }

    /** A second tap on the same status clears it. */
    fun toggleOnuStatus(status: String) = _state.update { it.copy(onuStatus = if (it.onuStatus == status) "" else status) }

    /** A second tap on the same OLT clears it. */
    fun toggleOnuOlt(id: String) = _state.update { it.copy(onuOltId = if (it.onuOltId == id) "" else id) }

    fun setOtherQuery(v: String) = _state.update { it.copy(otherQuery = v) }
}
