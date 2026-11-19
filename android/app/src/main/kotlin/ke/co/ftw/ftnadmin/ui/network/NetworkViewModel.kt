package ke.co.ftw.ftnadmin.ui.network

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import ke.co.ftw.ftnadmin.data.accounts.AccountsRepository
import ke.co.ftw.ftnadmin.data.routers.Client
import ke.co.ftw.ftnadmin.data.routers.ClientsResponse
import ke.co.ftw.ftnadmin.data.routers.NetInterface
import ke.co.ftw.ftnadmin.data.routers.RouterLiveRepository
import ke.co.ftw.ftnadmin.data.sites.NewDevicesRepository
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.common.apiResult
import ke.co.ftw.ftnadmin.ui.common.userMessage
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.FlowPreview
import kotlinx.coroutines.Job
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.collectLatest
import kotlinx.coroutines.flow.debounce
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.flow.drop
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch

enum class NetworkTab(val label: String) { Clients("Clients"), Interfaces("Interfaces") }

/** Filter chips of the clients list. Each maps to the server's `kind` / `online` query. */
enum class ClientFilter(val label: String, val kind: String?, val online: Boolean?) {
    All("All", null, null),
    Flats("Flats", "flats", null),
    Unregistered("Unregistered", "unregistered", null),
    Online("Online", null, true),
}

data class NetworkUiState(
    val routerId: String? = null,
    val tab: NetworkTab = NetworkTab.Clients,
    val query: String = "",
    val filter: ClientFilter = ClientFilter.All,
    val clients: Loadable<ClientsResponse> = Loadable.Loading,
    val interfaces: Loadable<List<NetInterface>> = Loadable.Loading,
    /** Key of the expanded row (client or interface), if any. */
    val expanded: String? = null,
    val refreshing: Boolean = false,
    /** Account or MAC an action is running for. */
    val busy: String? = null,
    val message: String? = null,
)

/** Network tab: clients (search, filters, expandable rows with actions) and interfaces of the selected router. */
@OptIn(FlowPreview::class, ExperimentalCoroutinesApi::class)
class NetworkViewModel(
    private val repo: RouterLiveRepository,
    private val accounts: AccountsRepository,
    private val newDevices: NewDevicesRepository,
    selection: StateFlow<String?>,
    private val searchDebounceMs: Long = 300,
) : ViewModel() {
    private val _state = MutableStateFlow(NetworkUiState(routerId = selection.value))
    val state: StateFlow<NetworkUiState> = _state.asStateFlow()
    private var clientsJob: Job? = null
    private var ifaceJob: Job? = null

    init {
        viewModelScope.launch {
            selection.collectLatest { id ->
                _state.update { NetworkUiState(routerId = id, tab = it.tab, query = it.query, filter = it.filter) }
                if (id != null) {
                    loadClients()
                    loadInterfaces()
                }
            }
        }
        viewModelScope.launch {
            _state.map { it.query }.distinctUntilChanged().drop(1).debounce(searchDebounceMs).collect { loadClients() }
        }
    }

    fun setTab(t: NetworkTab) = _state.update { it.copy(tab = t, expanded = null) }

    fun onQuery(q: String) = _state.update { it.copy(query = q) }

    fun setFilter(f: ClientFilter) {
        if (_state.value.filter == f) return
        _state.update { it.copy(filter = f, expanded = null) }
        loadClients()
    }

    fun toggle(key: String) = _state.update { it.copy(expanded = if (it.expanded == key) null else key) }

    fun retry() {
        loadClients()
        loadInterfaces()
    }

    fun refresh() {
        _state.update { it.copy(refreshing = true) }
        loadClients(initial = false)
        loadInterfaces(initial = false)
    }

    fun clearMessage() = _state.update { it.copy(message = null) }

    private fun loadClients(initial: Boolean = true) {
        val s = _state.value
        val id = s.routerId ?: return
        clientsJob?.cancel()
        if (initial && s.clients !is Loadable.Ready) _state.update { it.copy(clients = Loadable.Loading) }
        clientsJob = viewModelScope.launch {
            val r = apiResult { repo.clients(id, query = s.query.ifBlank { null }, kind = s.filter.kind, online = s.filter.online) }
            _state.update { cur ->
                if (cur.routerId != id) return@update cur
                r.fold(
                    { c -> cur.copy(clients = Loadable.Ready(c), refreshing = false) },
                    { e ->
                        if (cur.clients is Loadable.Ready) cur.copy(refreshing = false, message = e.userMessage())
                        else cur.copy(clients = Loadable.Failed(e.userMessage()), refreshing = false)
                    },
                )
            }
        }
    }

    private fun loadInterfaces(initial: Boolean = true) {
        val id = _state.value.routerId ?: return
        ifaceJob?.cancel()
        if (initial && _state.value.interfaces !is Loadable.Ready) _state.update { it.copy(interfaces = Loadable.Loading) }
        ifaceJob = viewModelScope.launch {
            val r = apiResult { repo.interfaces(id) }
            _state.update { cur ->
                if (cur.routerId != id) return@update cur
                r.fold(
                    { l -> cur.copy(interfaces = Loadable.Ready(l), refreshing = false) },
                    { e ->
                        if (cur.interfaces is Loadable.Ready) cur.copy(refreshing = false, message = e.userMessage())
                        else cur.copy(interfaces = Loadable.Failed(e.userMessage()), refreshing = false)
                    },
                )
            }
        }
    }

    // ---- row actions ----

    fun suspendToggle(c: Client) = act(c.key, if (c.suspendedOnRouter) "Unsuspended." else "Suspended.") {
        if (c.suspendedOnRouter) accounts.unsuspendAccount(c.accountId) else accounts.suspendAccount(c.accountId)
    }

    fun resync(c: Client) = act(c.key, "Resync queued.") { accounts.reconcile(c.accountId) }

    fun dismiss(c: Client) {
        val id = _state.value.routerId ?: return
        act(c.key, "Device dismissed.") { newDevices.dismiss(id, c.mac) }
    }

    private fun act(key: String, done: String, call: suspend () -> Any?) {
        if (_state.value.busy != null) return
        _state.update { it.copy(busy = key) }
        viewModelScope.launch {
            val r = apiResult { call() }
            _state.update { it.copy(busy = null, message = r.fold({ done }, { e -> e.userMessage() })) }
            if (r.isSuccess) loadClients(initial = false)
        }
    }
}
