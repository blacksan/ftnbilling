package ke.co.ftw.ftnadmin.ui.newdevices

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import ke.co.ftw.ftnadmin.core.network.ApiError
import ke.co.ftw.ftnadmin.core.network.ErrorCode
import ke.co.ftw.ftnadmin.core.network.MacOwner
import ke.co.ftw.ftnadmin.data.accounts.AccountsRepository
import ke.co.ftw.ftnadmin.data.sites.NewDevicesRepository
import ke.co.ftw.ftnadmin.data.sites.SitesRepository
import ke.co.ftw.ftnadmin.domain.Account
import ke.co.ftw.ftnadmin.domain.NewDevice
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
    val flat: String = "",
    val tenant: String = "",
    val phone: String = "",
    val packageId: String? = null,
    val busy: Boolean = false,
    val error: String? = null,
    /** 409 mac_in_use: who owns the MAC. Shown in plain words, never as a generic error. */
    val macOwner: MacOwner? = null,
    val confirmDismiss: Boolean = false,
)

data class NewDevicesUiState(
    val devices: Loadable<List<NewDevice>> = Loadable.Loading,
    val refreshing: Boolean = false,
    val sheet: DeviceSheet? = null,
    val packages: List<ServicePackage> = emptyList(),
    val message: String? = null,
)

class NewDevicesViewModel(
    private val siteId: String?,
    private val repo: NewDevicesRepository,
    private val sites: SitesRepository,
    private val accounts: AccountsRepository,
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

    fun open(d: NewDevice) = _state.update {
        it.copy(
            sheet = DeviceSheet(
                device = d,
                selected = d.suggestedAccount?.let { s -> Account(id = s.id, code = s.code, flat = s.flat, siteId = d.routerKey) },
            ),
        )
    }

    fun closeSheet() = _state.update { it.copy(sheet = null) }

    private fun edit(t: (DeviceSheet) -> DeviceSheet) = _state.update { s -> s.sheet?.let { s.copy(sheet = t(it).copy(error = null, macOwner = null)) } ?: s }

    fun setTab(t: SheetTab) = edit { it.copy(tab = t, confirmDismiss = false) }

    fun select(a: Account) = edit { it.copy(selected = a) }

    fun setFlat(v: String) = edit { it.copy(flat = v) }

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
        run(sh, "MAC attached to flat ${acc.flat}") { repo.attach(sh.device.routerKey, sh.device.mac, acc.id) }
    }

    fun create() {
        val sh = _state.value.sheet ?: return
        if (sh.flat.isBlank()) return
        run(sh, "Flat ${sh.flat.trim()} created") {
            repo.create(sh.device.routerKey, sh.device.mac, sh.flat.trim(), sh.tenant.trim(), sh.phone.trim(), sh.packageId)
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
        _state.update { it.copy(sheet = sh.copy(busy = true, error = null, macOwner = null)) }
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
                                error = when {
                                    api?.code == ErrorCode.MAC_IN_USE && api.macOwner != null -> null
                                    api?.code == ErrorCode.FLAT_IN_USE -> "Flat ${sh.flat.trim()} already exists on this router. Use Attach instead."
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
