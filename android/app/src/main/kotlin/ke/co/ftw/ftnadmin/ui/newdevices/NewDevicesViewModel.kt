package ke.co.ftw.ftnadmin.ui.newdevices

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import ke.co.ftw.ftnadmin.core.network.ApiError
import ke.co.ftw.ftnadmin.core.network.ErrorCode
import ke.co.ftw.ftnadmin.core.network.MacOwner
import ke.co.ftw.ftnadmin.data.accounts.AccountsRepository
import ke.co.ftw.ftnadmin.data.apartments.ApartmentsRepository
import ke.co.ftw.ftnadmin.domain.Apartment
import ke.co.ftw.ftnadmin.domain.BelongsTo
import ke.co.ftw.ftnadmin.data.sites.NewDevicesRepository
import ke.co.ftw.ftnadmin.data.sites.SitesRepository
import ke.co.ftw.ftnadmin.domain.Account
import ke.co.ftw.ftnadmin.domain.NewDevice
import ke.co.ftw.ftnadmin.domain.PendingRegistration
import ke.co.ftw.ftnadmin.domain.ServicePackage
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.common.apiResult
import ke.co.ftw.ftnadmin.ui.common.userMessage
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch

/** Online first, then most recently seen. ISO-8601 UTC strings sort chronologically. */
internal fun sortDevices(list: List<NewDevice>): List<NewDevice> =
    list.sortedWith(compareByDescending<NewDevice> { it.online }.thenByDescending { it.lastSeen.orEmpty() })

enum class SheetTab { Attach, Create }

data class DeviceSheet(
    val device: NewDevice,
    val tab: SheetTab = SheetTab.Attach,
    val selected: Account? = null,
    /** The unit (flat) in the chosen apartment. */
    val flat: String = "",
    /** Apartments the device's router serves, and the one picked for a new account. */
    val apartments: List<Apartment> = emptyList(),
    val apartmentId: String? = null,
    val tenant: String = "",
    val phone: String = "",
    val packageId: String? = null,
    val busy: Boolean = false,
    val error: String? = null,
    /** 409 mac_in_use: who owns the MAC. Shown in plain words, never as a generic error. */
    val macOwner: MacOwner? = null,
    val confirmDismiss: Boolean = false,
    /** 409 mac_other_building from Attach or Create: the customer who owns the MAC elsewhere. */
    val macOther: BelongsTo? = null,
    /** "Move customer here": the apartment to move into (when the router serves several) and the confirm step. */
    val moveApartmentId: String? = null,
    val confirmMove: Boolean = false,
)

data class NewDevicesUiState(
    val devices: Loadable<List<NewDevice>> = Loadable.Loading,
    val refreshing: Boolean = false,
    val sheet: DeviceSheet? = null,
    val packages: List<ServicePackage> = emptyList(),
    val message: String? = null,
    /** People who registered on the captive page and wait for approval. */
    val registrations: List<PendingRegistration> = emptyList(),
    /** The registration being approved (its device picker is open) and the MAC chosen for it. */
    val approving: PendingRegistration? = null,
    val approveMac: String = "",
    val rejecting: PendingRegistration? = null,
    /** "Reject all" waits for a confirm: which router's queue it would empty. */
    val rejectingAll: RejectAllTarget? = null,
    val decideBusy: Boolean = false,
)

/** The waiting registrations of one router ([siteId] blank = the ones that name no router yet), for "Reject all". */
data class RejectAllTarget(val siteId: String, val label: String, val count: Int)

/** The queues the owner can empty at once: one per router (blank site = no router yet), with how many wait. */
internal fun rejectAllTargets(list: List<PendingRegistration>): List<RejectAllTarget> =
    list.groupBy { it.siteId }.map { (site, rows) ->
        RejectAllTarget(site, if (site.isBlank()) "no router yet" else rows.first().siteName.ifBlank { "this router" }, rows.size)
    }.sortedBy { it.label }

class NewDevicesViewModel(
    private val siteId: String?,
    private val repo: NewDevicesRepository,
    private val sites: SitesRepository,
    private val accounts: AccountsRepository,
    private val apartmentsRepo: ApartmentsRepository? = null,
) : ViewModel() {
    private val _state = MutableStateFlow(NewDevicesUiState())
    val state: StateFlow<NewDevicesUiState> = _state.asStateFlow()

    init {
        load(initial = true)
        viewModelScope.launch {
            apiResult { accounts.packages(siteId) }.getOrNull()?.let { l -> _state.update { it.copy(packages = l.filterNot { p -> p.disabled }) } }
        }
    }

    fun retry() = load(initial = true)

    /** Pull-to-refresh: asks the server to re-read the router(s), then reloads the list. */
    fun refresh() {
        _state.update { it.copy(refreshing = true) }
        viewModelScope.launch {
            val ids = siteId?.let { listOf(it) } ?: apiResult { sites.sites() }.getOrNull()?.map { it.key }.orEmpty()
            // Best effort: one unreachable router must not stop the others or the reload.
            ids.forEach { apiResult { repo.refresh(it) } }
            fetch()
        }
    }

    private fun load(initial: Boolean) {
        if (initial) _state.update { it.copy(devices = Loadable.Loading) }
        viewModelScope.launch { fetch() }
    }

    private suspend fun fetch() {
        // Best effort: the waiting list must never stop the device list from showing.
        apiResult { repo.registrations(siteId) }.getOrNull()?.let { l -> _state.update { it.copy(registrations = l) } }
        val r = apiResult { repo.list(siteId) }
        _state.update { cur ->
            r.fold(
                { l -> cur.copy(devices = Loadable.Ready(sortDevices(l)), refreshing = false) },
                { e ->
                    if (cur.devices is Loadable.Ready) cur.copy(refreshing = false, message = e.userMessage())
                    else cur.copy(devices = Loadable.Failed(e.userMessage()), refreshing = false)
                },
            )
        }
    }

    fun clearMessage() = _state.update { it.copy(message = null) }

    fun askApprove(r: PendingRegistration) =
        _state.update { it.copy(approving = r, approveMac = r.mac.ifBlank { r.guessMac }) }

    fun setApproveMac(mac: String) = _state.update { it.copy(approveMac = mac) }

    fun cancelApprove() = _state.update { it.copy(approving = null, approveMac = "") }

    fun askReject(r: PendingRegistration) = _state.update { it.copy(rejecting = r) }

    fun cancelReject() = _state.update { it.copy(rejecting = null) }

    fun askRejectAll(t: RejectAllTarget) = _state.update { it.copy(rejectingAll = t) }

    fun cancelRejectAll() = _state.update { it.copy(rejectingAll = null) }

    fun rejectAll() {
        val t = _state.value.rejectingAll ?: return
        if (_state.value.decideBusy) return
        _state.update { it.copy(decideBusy = true) }
        viewModelScope.launch {
            apiResult { repo.rejectAllRegistrations(t.siteId) }.fold(
                {
                    _state.update { it.copy(rejectingAll = null, decideBusy = false, message = "Rejected the waiting registrations of ${t.label}. The details were erased.") }
                    fetch()
                },
                { e -> _state.update { it.copy(rejectingAll = null, decideBusy = false, message = e.userMessage()) } },
            )
        }
    }

    fun approve() {
        val r = _state.value.approving ?: return
        val mac = _state.value.approveMac.trim()
        if (mac.isEmpty() || _state.value.decideBusy) return
        _state.update { it.copy(decideBusy = true) }
        viewModelScope.launch {
            apiResult { repo.approveRegistration(r.id, mac) }.fold(
                {
                    _state.update { it.copy(approving = null, approveMac = "", decideBusy = false, message = "Approved. House ${r.unit} is set up.") }
                    fetch()
                },
                { e -> _state.update { it.copy(decideBusy = false, message = e.userMessage()) } },
            )
        }
    }

    fun reject() {
        val r = _state.value.rejecting ?: return
        if (_state.value.decideBusy) return
        _state.update { it.copy(decideBusy = true) }
        viewModelScope.launch {
            apiResult { repo.rejectRegistration(r.id) }.fold(
                {
                    _state.update { it.copy(rejecting = null, decideBusy = false, message = "Registration rejected. The details were erased.") }
                    fetch()
                },
                { e -> _state.update { it.copy(decideBusy = false, message = e.userMessage()) } },
            )
        }
    }

    fun open(d: NewDevice) {
        val b = d.belongsTo
        _state.update {
            it.copy(
                sheet = DeviceSheet(
                    device = d,
                    selected = d.suggestedAccount?.let { s -> Account(id = s.id, code = s.code, flat = s.flat, siteId = d.routerKey) },
                    // The only apartment the router could move the customer into is preselected.
                    moveApartmentId = b?.moveToChoices?.singleOrNull()?.id,
                ),
            )
        }
        if (b == null) loadApartments(d)
    }

    /** The apartments the device's router serves: a new account must be in one of them. */
    private fun loadApartments(d: NewDevice) {
        val aRepo = apartmentsRepo ?: return
        viewModelScope.launch {
            val l = apiResult { aRepo.list(routerId = d.routerKey) }.getOrNull() ?: return@launch
            _state.update { s ->
                val sh = s.sheet ?: return@update s
                if (sh.device.mac != d.mac) return@update s
                s.copy(sheet = sh.copy(apartments = l, apartmentId = sh.apartmentId ?: l.singleOrNull()?.id))
            }
        }
    }

    fun closeSheet() = _state.update { it.copy(sheet = null) }

    private fun edit(t: (DeviceSheet) -> DeviceSheet) =
        _state.update { s -> s.sheet?.let { s.copy(sheet = t(it).copy(error = null, macOwner = null, macOther = null)) } ?: s }

    fun setTab(t: SheetTab) = edit { it.copy(tab = t, confirmDismiss = false) }

    fun select(a: Account) = edit { it.copy(selected = a) }

    fun setFlat(v: String) = edit { it.copy(flat = v) }

    fun setApartment(id: String) = edit { it.copy(apartmentId = id) }

    fun setMoveApartment(id: String) = edit { it.copy(moveApartmentId = id, confirmMove = false) }

    fun askMove() = _state.update { s -> s.sheet?.let { s.copy(sheet = it.copy(confirmMove = true)) } ?: s }

    fun cancelMove() = _state.update { s -> s.sheet?.let { s.copy(sheet = it.copy(confirmMove = false)) } ?: s }

    /**
     * The device belongs to a customer of another apartment. After the operator's confirm the customer follows it
     * to this router: the old router's lease and queue go, the new router gets them, the history says so.
     */
    fun moveHere() {
        val sh = _state.value.sheet ?: return
        val b = sh.device.belongsTo ?: return
        run(sh.copy(confirmMove = false), "${b.customer.ifBlank { "Customer" }} moved to this router") {
            repo.moveCustomerHere(sh.device.routerKey, sh.device.mac, sh.moveApartmentId)
        }
    }

    fun setTenant(v: String) = edit { it.copy(tenant = v) }

    fun setPhone(v: String) = edit { it.copy(phone = v) }

    fun setPackage(id: String) = edit { it.copy(packageId = if (it.packageId == id) null else id) }

    fun askDismiss() = _state.update { s -> s.sheet?.let { s.copy(sheet = it.copy(confirmDismiss = true)) } ?: s }

    fun cancelDismiss() = _state.update { s -> s.sheet?.let { s.copy(sheet = it.copy(confirmDismiss = false)) } ?: s }

    /** Accounts of the device's router, those without a MAC first (the likeliest targets). */
    suspend fun searchAccounts(q: String): List<Account> {
        val site = _state.value.sheet?.device?.routerKey ?: siteId
        return accounts.list(siteId = site, query = q.ifBlank { null }, limit = 50).items
            .sortedWith(compareBy<Account> { it.mac.isNotBlank() }.thenBy { it.flat })
    }

    fun attach() {
        val sh = _state.value.sheet ?: return
        val acc = sh.selected ?: return
        run(sh, "MAC attached to house ${acc.flat}") { repo.attach(sh.device.routerKey, sh.device.mac, acc.id) }
    }

    fun create() {
        val sh = _state.value.sheet ?: return
        val apt = sh.apartmentId ?: return
        if (sh.flat.isBlank()) return
        run(sh, "${sh.flat.trim()} created") {
            repo.create(sh.device.routerKey, apt, sh.device.mac, sh.flat.trim(), sh.tenant.trim(), sh.phone.trim(), sh.packageId)
        }
    }

    fun dismissDevice() {
        val sh = _state.value.sheet ?: return
        _state.update { it.copy(sheet = sh.copy(busy = true, error = null)) }
        viewModelScope.launch {
            val r = apiResult { repo.dismiss(sh.device.routerKey, sh.device.mac) }
            r.fold(
                {
                    _state.update { it.copy(sheet = null, message = "Device dismissed") }
                    fetch()
                },
                { e -> _state.update { it.copy(sheet = sh.copy(busy = false, error = e.userMessage(), confirmDismiss = false)) } },
            )
        }
    }

    private fun run(sh: DeviceSheet, done: String, block: suspend () -> Account) {
        _state.update { it.copy(sheet = sh.copy(busy = true, error = null, macOwner = null, macOther = null)) }
        viewModelScope.launch {
            val r = apiResult(block)
            r.fold(
                {
                    _state.update { it.copy(sheet = null, message = done) }
                    fetch()
                },
                { e ->
                    val api = e as? ApiError
                    _state.update {
                        it.copy(
                            sheet = sh.copy(
                                busy = false,
                                macOwner = if (api?.code == ErrorCode.MAC_IN_USE) api.macOwner else null,
                                macOther = if (api?.code == ErrorCode.MAC_OTHER_BUILDING) api.macOtherBuilding else null,
                                error = when {
                                    api?.code == ErrorCode.MAC_IN_USE && api.macOwner != null -> null
                                    api?.code == ErrorCode.MAC_OTHER_BUILDING && api.macOtherBuilding != null -> null
                                    api?.code == ErrorCode.UNIT_IN_USE -> "House number ${sh.flat.trim()} is already used in this apartment. Use Attach instead."
                                    api?.code == ErrorCode.FLAT_IN_USE -> "House number ${sh.flat.trim()} already exists on this router. Use Attach instead."
                                    else -> e.userMessage()
                                },
                            ),
                        )
                    }
                },
            )
        }
    }
}
