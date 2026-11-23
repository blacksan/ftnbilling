package ke.co.ftw.ftnadmin.ui.equipment

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import ke.co.ftw.ftnadmin.core.network.ApiError
import ke.co.ftw.ftnadmin.core.network.ErrorCode
import ke.co.ftw.ftnadmin.data.apartments.ApartmentsRepository
import ke.co.ftw.ftnadmin.data.equipment.EquipmentRepository
import ke.co.ftw.ftnadmin.data.equipment.Olt
import ke.co.ftw.ftnadmin.data.equipment.OltInput
import ke.co.ftw.ftnadmin.data.sites.SitesRepository
import ke.co.ftw.ftnadmin.domain.Apartment
import ke.co.ftw.ftnadmin.domain.MacAddress
import ke.co.ftw.ftnadmin.domain.Site
import ke.co.ftw.ftnadmin.ui.common.apiResult
import ke.co.ftw.ftnadmin.ui.common.userMessage
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch

/** The most PON ports an OLT can have; the server refuses more. */
const val MAX_PON_PORTS = 64

data class OltForm(
    val name: String = "",
    val mac: String = "",
    val model: String = "",
    val serial: String = "",
    val siteId: String = "",
    val routerPort: String = "",
    val ponPorts: String = "",
    /** Apartments served by each PON port. */
    val ponApartments: Map<Int, Set<String>> = emptyMap(),
    val mgmtIp: String = "",
    val mgmtUser: String = "",
    /** Typed password: write-only, never filled from the server. */
    val mgmtPass: String = "",
    val clearMgmtPass: Boolean = false,
    val errors: Map<String, String> = emptyMap(),
)

data class OltFormUiState(
    val loading: Boolean = true,
    val form: OltForm = OltForm(),
    val routers: List<Site> = emptyList(),
    /** The apartments of the chosen router, for the per-port pickers. */
    val apartments: List<Apartment> = emptyList(),
    /** An OLT being edited already has a stored password: the field says so and stays blank. */
    val hadPass: Boolean = false,
    val busy: Boolean = false,
    val error: String? = null,
    val saved: Olt? = null,
) {
    val ports: List<Int> get() = (1..(form.ponPorts.toIntOrNull() ?: 0).coerceIn(0, MAX_PON_PORTS)).toList()
}

/** Add an OLT (oltId null) or edit one. The MAC is required; the password goes to the server and is never shown again. */
class OltFormViewModel(
    private val oltId: String?,
    private val repo: EquipmentRepository,
    private val sites: SitesRepository,
    private val apartmentsRepo: ApartmentsRepository,
    initialSiteId: String? = null,
) : ViewModel() {
    private val _state = MutableStateFlow(OltFormUiState(form = OltForm(siteId = initialSiteId.orEmpty())))
    val state: StateFlow<OltFormUiState> = _state.asStateFlow()

    init {
        viewModelScope.launch {
            apiResult { sites.sites() }.getOrNull()?.let { l -> _state.update { it.copy(routers = l) } }
            if (oltId == null) {
                _state.update { it.copy(loading = false) }
                loadApartments(_state.value.form.siteId)
                return@launch
            }
            val r = apiResult { repo.olt(oltId) }
            r.fold(
                { o ->
                    _state.update {
                        it.copy(
                            loading = false,
                            hadPass = o.hasMgmtPass,
                            form = OltForm(
                                name = o.name, mac = o.mac, model = o.model, serial = o.serial, siteId = o.siteId, routerPort = o.routerPort,
                                ponPorts = if (o.ponPorts > 0) o.ponPorts.toString() else "",
                                ponApartments = o.ponApartments.mapNotNull { (k, v) -> k.toIntOrNull()?.let { p -> p to v.toSet() } }.toMap(),
                                mgmtIp = o.mgmtIp, mgmtUser = o.mgmtUser,
                            ),
                        )
                    }
                    loadApartments(o.siteId)
                },
                { e -> _state.update { it.copy(loading = false, error = e.userMessage()) } },
            )
        }
    }

    private fun loadApartments(siteId: String) {
        if (siteId.isBlank()) return
        viewModelScope.launch {
            val l = apiResult { apartmentsRepo.list(routerId = siteId) }.getOrNull() ?: return@launch
            _state.update { if (it.form.siteId == siteId) it.copy(apartments = l) else it }
        }
    }

    private fun edit(f: OltForm.() -> OltForm) = _state.update { it.copy(form = it.form.f(), error = null) }

    fun onName(v: String) = edit { copy(name = v, errors = errors - "name") }

    fun onMac(v: String) = edit { copy(mac = v, errors = errors - "mac") }

    fun onModel(v: String) = edit { copy(model = v) }

    fun onSerial(v: String) = edit { copy(serial = v) }

    fun onRouterPort(v: String) = edit { copy(routerPort = v, errors = errors - "router_port") }

    fun onPonPorts(v: String) = edit {
        val digits = v.filter { it.isDigit() }.take(2)
        val n = digits.toIntOrNull() ?: 0
        // Ports that no longer exist lose their apartments, like the web form.
        copy(ponPorts = digits, ponApartments = ponApartments.filterKeys { it <= n }, errors = errors - "pon_ports")
    }

    fun onMgmtIp(v: String) = edit { copy(mgmtIp = v, errors = errors - "mgmt_ip") }

    fun onMgmtUser(v: String) = edit { copy(mgmtUser = v) }

    fun onMgmtPass(v: String) = edit { copy(mgmtPass = v, clearMgmtPass = false, errors = errors - "mgmt_pass") }

    fun onClearPass(v: Boolean) = edit { copy(clearMgmtPass = v, mgmtPass = if (v) "" else mgmtPass) }

    fun onRouter(id: String) {
        // The apartments belong to the router: a new router starts the per-port choices again.
        edit { copy(siteId = id, ponApartments = emptyMap(), errors = errors - "site_id") }
        _state.update { it.copy(apartments = emptyList()) }
        loadApartments(id)
    }

    /** Ticks or unticks [apartmentId] on PON [port]. */
    fun toggleApartment(port: Int, apartmentId: String) = edit {
        val cur = ponApartments[port].orEmpty()
        val next = if (apartmentId in cur) cur - apartmentId else cur + apartmentId
        copy(ponApartments = if (next.isEmpty()) ponApartments - port else ponApartments + (port to next))
    }

    /** Field problems of [f]; empty when it can be sent. */
    fun validate(f: OltForm): Map<String, String> = buildMap {
        if (f.name.isBlank()) put("name", "Give the OLT a name.")
        if (MacAddress.normalize(f.mac) == null) put("mac", MacAddress.errorFor(f.mac) ?: "Enter the OLT's MAC address.")
        if (f.siteId.isBlank()) put("site_id", "Choose the router it connects to.")
        val n = f.ponPorts.toIntOrNull()
        if (f.ponPorts.isNotBlank() && (n == null || n < 0 || n > MAX_PON_PORTS)) put("pon_ports", "Use a number from 0 to $MAX_PON_PORTS.")
    }

    fun save() {
        val f = _state.value.form
        val problems = validate(f)
        if (problems.isNotEmpty()) {
            _state.update { it.copy(form = it.form.copy(errors = problems)) }
            return
        }
        val mac = MacAddress.normalize(f.mac) ?: return
        val ports = f.ponPorts.toIntOrNull() ?: 0
        val input = OltInput(
            name = f.name, mac = mac, model = f.model, serial = f.serial, siteId = f.siteId, routerPort = f.routerPort, ponPorts = ports,
            ponApartments = f.ponApartments.filterKeys { it in 1..ports }.mapValues { it.value.sorted() },
            mgmtIp = f.mgmtIp, mgmtUser = f.mgmtUser, mgmtPass = f.mgmtPass, clearMgmtPass = f.clearMgmtPass,
        )
        _state.update { it.copy(busy = true, error = null) }
        viewModelScope.launch {
            val r = apiResult { repo.saveOlt(oltId, input) }
            r.fold(
                { o -> _state.update { it.copy(busy = false, saved = o, form = it.form.copy(mgmtPass = "")) } },
                { e ->
                    val api = e as? ApiError
                    _state.update { cur ->
                        val fe = api?.fieldErrors.orEmpty().toMutableMap()
                        // 409 mac_in_use: another device or an account owns this MAC.
                        if (api?.code == ErrorCode.MAC_IN_USE) fe["mac"] = api.message
                        if (fe.isNotEmpty()) cur.copy(busy = false, form = cur.form.copy(errors = cur.form.errors + fe))
                        else cur.copy(busy = false, error = e.userMessage())
                    }
                },
            )
        }
    }
}
