package ke.co.ftw.ftnadmin.ui.customers.add

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import ke.co.ftw.ftnadmin.core.network.ApiError
import ke.co.ftw.ftnadmin.core.network.ErrorCode
import ke.co.ftw.ftnadmin.core.network.MacOwner
import ke.co.ftw.ftnadmin.data.accounts.AccountsRepository
import ke.co.ftw.ftnadmin.data.apartments.ApartmentsRepository
import ke.co.ftw.ftnadmin.domain.Apartment
import ke.co.ftw.ftnadmin.domain.BelongsTo
import ke.co.ftw.ftnadmin.data.routers.Client
import ke.co.ftw.ftnadmin.data.routers.RouterLiveRepository
import ke.co.ftw.ftnadmin.data.sites.SitesRepository
import ke.co.ftw.ftnadmin.domain.Account
import ke.co.ftw.ftnadmin.domain.MacAddress
import ke.co.ftw.ftnadmin.domain.ServicePackage
import ke.co.ftw.ftnadmin.domain.Site
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.common.apiResult
import ke.co.ftw.ftnadmin.ui.common.userMessage
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch

/** What the router says about a new account, in the words the operator sees. */
sealed interface ProvisionStatus {
    /** The server is still pushing the account to the router. */
    data object Working : ProvisionStatus

    /** On the router: lease, queue and walled state are in place. */
    data object Provisioned : ProvisionStatus

    /** Router offline or busy: the change is queued and applies by itself when the router is back. */
    data object Queued : ProvisionStatus

    data class Failed(val reason: String) : ProvisionStatus

    val label: String
        get() = when (this) {
            Working -> "Provisioning on the router..."
            Provisioned -> "Provisioned on router"
            Queued -> "Queued - applies automatically when the router is back"
            is Failed -> "Provisioning problem"
        }
}

/** Maps the account's router state to a status. [routerOnline] is null when the router list has not loaded. */
fun provisionStatus(account: Account, routerOnline: Boolean?): ProvisionStatus {
    val r = account.router ?: return ProvisionStatus.Working
    return when {
        r.lastError.isNotBlank() && !r.pendingTask -> ProvisionStatus.Failed(r.lastError)
        r.pendingTask || !r.inSync -> if (routerOnline == false) ProvisionStatus.Queued else ProvisionStatus.Working
        else -> ProvisionStatus.Provisioned
    }
}

data class AddedCustomer(val account: Account, val status: ProvisionStatus)

data class AddCustomerUiState(
    val tenant: String = "",
    val phone: String = "",
    /** Apartment (building) and the unit in it: the customer lives in one apartment, in one unit. */
    val apartments: Loadable<List<Apartment>> = Loadable.Loading,
    val apartmentId: String? = null,
    val unit: String = "",
    val routers: List<Site> = emptyList(),
    val routerId: String? = null,
    val packages: Loadable<List<ServicePackage>> = Loadable.Loading,
    val packageId: String? = null,
    val macInput: String = "",
    val deviceQuery: String = "",
    val devices: Loadable<List<Client>> = Loadable.Loading,
    val activate: Boolean = true,
    val submitting: Boolean = false,
    val error: String? = null,
    /** Set on a 409 `mac_in_use`: names the flat and router that already own the MAC. */
    val macOwner: MacOwner? = null,
    /** Set on a 409 `mac_other_building`: the MAC belongs to a customer of another apartment. */
    val macOther: BelongsTo? = null,
    val added: AddedCustomer? = null,
) {
    val apartment: Apartment? get() = (apartments as? Loadable.Ready)?.value?.firstOrNull { it.id == apartmentId }

    /** The routers of the chosen apartment. One: it is used automatically. Several: the operator picks. */
    val apartmentRouters: List<Site>
        get() {
            val keys = apartment?.routers?.map { it.key }.orEmpty().toSet()
            val known = routers.filter { it.key in keys }
            // A router the router list does not know yet still counts: the apartment names it.
            val missing = apartment?.routers.orEmpty().filter { r -> routers.none { it.key == r.key } }.map { Site(id = it.key, name = it.label, routerId = it.key, routerName = it.label) }
            return known + missing
        }
    val needsRouterChoice: Boolean get() = apartmentRouters.size > 1

    val macError: String? get() = MacAddress.errorFor(macInput)
    val macValid: Boolean get() = MacAddress.isValid(macInput)
    val canSubmit: Boolean
        get() = !submitting && added == null && unit.isNotBlank() && apartmentId != null && routerId != null && packageId != null && macValid

    /** Devices the router sees now, filtered by the search text. */
    val shownDevices: List<Client>
        get() = ((devices as? Loadable.Ready)?.value.orEmpty()).filter { it.matches(deviceQuery) }
            .sortedWith(compareByDescending<Client> { it.online }.thenBy { it.mac })
}

/**
 * Add customer: one screen, name to MAC, then the result with live provisioning status.
 * The router is preselected from the top-bar switcher; the MAC is picked from devices the router
 * sees now or pasted (validated like the server does).
 */
class AddCustomerViewModel(
    private val accounts: AccountsRepository,
    private val live: RouterLiveRepository,
    private val sites: SitesRepository,
    private val apartmentsRepo: ApartmentsRepository,
    initialRouterId: String?,
    initialMac: String?,
    private val pollMs: Long = 2_000,
    private val maxPolls: Int = 15,
) : ViewModel() {
    private val _state = MutableStateFlow(AddCustomerUiState(routerId = initialRouterId, macInput = initialMac.orEmpty()))
    val state: StateFlow<AddCustomerUiState> = _state.asStateFlow()
    private var pollJob: Job? = null

    init {
        viewModelScope.launch {
            apiResult { sites.sites() }.getOrNull()?.let { l -> _state.update { cur -> cur.copy(routers = l) } }
            val r = apiResult { apartmentsRepo.list() }
            _state.update { cur -> cur.copy(apartments = r.fold({ l -> Loadable.Ready(l) }, { e -> Loadable.Failed(e.userMessage()) })) }
            // Coming from a router or a device: the apartments that router serves. One of them is chosen for you.
            val from = _state.value.routerId
            val list = (_state.value.apartments as? Loadable.Ready)?.value.orEmpty()
            val fit = if (from != null) list.filter { a -> a.routers.any { it.key == from } } else list
            val pick = fit.singleOrNull() ?: list.singleOrNull()
            if (pick != null) selectApartment(pick.id, preferRouter = from)
        }
    }

    /** [forRouter] null: no router is chosen yet, the packages shown are those of the apartment's first router. */
    private suspend fun loadRouterData(id: String, forRouter: String? = id) {
        _state.update { it.copy(packages = Loadable.Loading, devices = Loadable.Loading) }
        val pk = apiResult { accounts.packages(id) }
        _state.update { cur ->
            if (cur.routerId != forRouter) return@update cur
            cur.copy(
                packages = pk.fold(
                    { l -> Loadable.Ready(l.filterNot { p -> p.disabled }.sortedBy { p -> p.priceKes }) },
                    { e -> Loadable.Failed(e.userMessage()) },
                ),
            )
        }
        if (forRouter != null) loadDevices(id) else _state.update { it.copy(devices = Loadable.Ready(emptyList())) }
    }

    private suspend fun loadDevices(id: String) {
        val r = apiResult { live.clients(id, kind = "unregistered") }
        _state.update { cur ->
            if (cur.routerId != id) return@update cur
            cur.copy(devices = r.fold({ c -> Loadable.Ready(c.items) }, { e -> Loadable.Failed(e.userMessage()) }))
        }
    }

    fun refreshDevices() {
        val id = _state.value.routerId ?: return
        _state.update { it.copy(devices = Loadable.Loading) }
        viewModelScope.launch { loadDevices(id) }
    }

    /** Choosing the apartment fixes the router when it has only one; the packages follow the router. */
    private fun selectApartment(id: String, preferRouter: String? = null) {
        val a = (_state.value.apartments as? Loadable.Ready)?.value?.firstOrNull { it.id == id } ?: return
        val keys = a.routers.map { it.key }
        val router = when {
            keys.size == 1 -> keys.first()
            preferRouter != null && preferRouter in keys -> preferRouter
            else -> null
        }
        _state.update { it.copy(apartmentId = id, routerId = router, packageId = null, macOwner = null, macOther = null, error = null) }
        val load = router ?: keys.firstOrNull()
        if (load != null) viewModelScope.launch { loadRouterData(load, forRouter = router) }
    }

    fun onApartment(id: String) {
        if (_state.value.apartmentId != id) selectApartment(id)
    }

    fun setRouter(id: String) {
        if (_state.value.routerId == id) return
        _state.update { it.copy(routerId = id, packageId = null, macOwner = null, macOther = null, error = null) }
        viewModelScope.launch { loadRouterData(id) }
    }

    fun onTenant(v: String) = _state.update { it.copy(tenant = v) }

    fun onPhone(v: String) = _state.update { it.copy(phone = v) }

    fun onUnit(v: String) = _state.update { it.copy(unit = v, error = null) }

    fun onPackage(id: String) = _state.update { it.copy(packageId = id) }

    fun onMac(v: String) = _state.update { it.copy(macInput = v, macOwner = null, macOther = null, error = null) }

    fun onDeviceQuery(v: String) = _state.update { it.copy(deviceQuery = v) }

    /** Picking a live device fills the MAC field. */
    fun pickDevice(c: Client) = _state.update { it.copy(macInput = c.mac, macOwner = null, macOther = null, error = null) }

    fun onActivate(v: Boolean) = _state.update { it.copy(activate = v) }

    fun submit() {
        val s = _state.value
        if (!s.canSubmit) return
        val mac = MacAddress.normalize(s.macInput) ?: return
        _state.update { it.copy(submitting = true, error = null, macOwner = null, macOther = null) }
        viewModelScope.launch {
            val r = apiResult {
                accounts.create(
                    apartmentId = s.apartmentId!!,
                    routerId = s.routerId,
                    unit = s.unit,
                    tenant = s.tenant,
                    phone = s.phone,
                    mac = mac,
                    packageId = s.packageId,
                    activate = s.activate,
                )
            }
            r.fold(
                { a ->
                    val status = provisionStatus(a, routerOnline(s.routerId))
                    _state.update { it.copy(submitting = false, added = AddedCustomer(a, status)) }
                    if (status == ProvisionStatus.Working) startPolling(a.id, s.routerId)
                },
                { e ->
                    val api = e as? ApiError
                    _state.update {
                        when {
                            api != null && api.code == ErrorCode.MAC_IN_USE && api.macOwner != null -> it.copy(submitting = false, macOwner = api.macOwner)
                            api != null && api.code == ErrorCode.MAC_OTHER_BUILDING && api.macOtherBuilding != null ->
                                it.copy(submitting = false, macOther = api.macOtherBuilding)
                            api != null && (api.code == ErrorCode.UNIT_IN_USE || api.code == ErrorCode.FLAT_IN_USE) ->
                                it.copy(submitting = false, error = "House number ${s.unit.trim()} is already used in this apartment. Open that customer instead.")
                            else -> it.copy(submitting = false, error = e.userMessage())
                        }
                    }
                },
            )
        }
    }

    private fun routerOnline(id: String?): Boolean? = _state.value.routers.firstOrNull { it.key == id }?.online

    /** Rereads the account until the router has it, or [maxPolls] tries pass (then it stays Queued). */
    private fun startPolling(accountId: String, routerId: String?) {
        pollJob?.cancel()
        pollJob = viewModelScope.launch {
            var n = 0
            while (isActive && n < maxPolls) {
                delay(pollMs)
                n++
                val r = apiResult { accounts.get(accountId) }.getOrNull() ?: continue
                val status = provisionStatus(r, routerOnline(routerId))
                _state.update { it.copy(added = AddedCustomer(r, status)) }
                if (status != ProvisionStatus.Working) return@launch
            }
            if (isActive) _state.update { cur -> cur.added?.let { cur.copy(added = it.copy(status = ProvisionStatus.Queued)) } ?: cur }
        }
    }

    /** "Check again" on the result card. */
    fun recheck() {
        val a = _state.value.added ?: return
        startPolling(a.account.id, _state.value.routerId)
    }

    /** Start another customer on the same router. */
    fun addAnother() {
        pollJob?.cancel()
        val id = _state.value.routerId
        _state.update {
            AddCustomerUiState(
                routers = it.routers, apartments = it.apartments, apartmentId = it.apartmentId, routerId = id,
                packages = it.packages, devices = it.devices,
            )
        }
        if (id != null) viewModelScope.launch { loadDevices(id) }
    }
}
