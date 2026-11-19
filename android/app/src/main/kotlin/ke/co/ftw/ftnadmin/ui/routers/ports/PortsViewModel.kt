package ke.co.ftw.ftnadmin.ui.routers.ports

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import ke.co.ftw.ftnadmin.data.routers.PortConfirm
import ke.co.ftw.ftnadmin.data.routers.PortRole
import ke.co.ftw.ftnadmin.data.routers.PortRow
import ke.co.ftw.ftnadmin.data.routers.PortRules
import ke.co.ftw.ftnadmin.data.routers.PortsResponse
import ke.co.ftw.ftnadmin.data.routers.RouterLiveRepository
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

/** A change waiting for the person's confirmation. */
data class PendingPortChange(
    val port: String,
    val role: String?,
    val disabled: Boolean?,
    val confirm: PortConfirm,
    val typed: String = "",
)

data class PortsUiState(
    val ports: Loadable<PortsResponse> = Loadable.Loading,
    val refreshing: Boolean = false,
    val busy: Set<String> = emptySet(),
    val pending: PendingPortChange? = null,
    val message: String? = null,
    val stale: Boolean = false,
)

/**
 * Ports of one router. Polls the server about every 5 s only while the screen is visible
 * (the server answers from a 4 s cache, so this is one /interface read on the router per poll).
 */
class PortsViewModel(
    private val routerId: String,
    private val routerName: String,
    private val repo: RouterLiveRepository,
    private val pollMs: Long = 5_000,
) : ViewModel() {
    private val _state = MutableStateFlow(PortsUiState())
    val state: StateFlow<PortsUiState> = _state.asStateFlow()

    private var active = false
    private var pollJob: Job? = null

    init {
        load(initial = true)
    }

    fun retry() = load(initial = true)

    fun refresh() {
        _state.update { it.copy(refreshing = true) }
        load(initial = false)
    }

    private fun load(initial: Boolean) {
        if (initial) _state.update { it.copy(ports = Loadable.Loading) }
        viewModelScope.launch { fetch() }
    }

    private suspend fun fetch() {
        val r = apiResult { repo.ports(routerId) }
        _state.update { cur ->
            r.fold(
                { p -> cur.copy(ports = Loadable.Ready(p), refreshing = false, stale = false) },
                { e ->
                    if (cur.ports is Loadable.Ready) cur.copy(refreshing = false, stale = true)
                    else cur.copy(ports = Loadable.Failed(e.userMessage()), refreshing = false)
                },
            )
        }
    }

    fun resume() {
        active = true
        if (pollJob?.isActive == true) return
        pollJob = viewModelScope.launch {
            while (isActive && active) {
                delay(pollMs)
                val s = _state.value
                // Do not move rows under a dialog or while a change is on its way.
                if (s.pending == null && s.busy.isEmpty()) fetch()
            }
        }
    }

    fun pause() {
        active = false
        pollJob?.cancel()
        pollJob = null
    }

    fun clearMessage() = _state.update { it.copy(message = null) }

    // ---- actions ----

    fun requestDisable(p: PortRow) {
        if (p.disabled) return apply(p.name, role = null, disabled = false, typed = null)
        val c = PortRules.disable(p)
        _state.update { it.copy(pending = PendingPortChange(p.name, null, true, c)) }
    }

    fun requestRole(p: PortRow, role: String) {
        if (role == p.role) return
        _state.update { it.copy(pending = PendingPortChange(p.name, role, null, PortRules.role(p, role))) }
    }

    fun setTyped(text: String) = _state.update { s -> s.copy(pending = s.pending?.copy(typed = text)) }

    fun cancel() = _state.update { it.copy(pending = null) }

    /** True when the typed router name matches (or no name is needed). */
    fun canConfirm(): Boolean {
        val p = _state.value.pending ?: return false
        return !p.confirm.needsName || p.typed.trim().equals(routerName.trim(), ignoreCase = true)
    }

    fun confirm() {
        val p = _state.value.pending ?: return
        if (!canConfirm()) return
        _state.update { it.copy(pending = null) }
        apply(p.port, p.role, p.disabled, if (p.confirm.needsName) p.typed.trim() else null)
    }

    private fun apply(port: String, role: String?, disabled: Boolean?, typed: String?) {
        _state.update { it.copy(busy = it.busy + port) }
        viewModelScope.launch {
            val r = apiResult { repo.changePort(routerId, port, role, disabled, typed) }
            r.fold(
                {
                    _state.update { it.copy(message = "${port}: saved. The server applies it on the router.") }
                },
                { e -> _state.update { it.copy(message = e.userMessage()) } },
            )
            fetch()
            _state.update { it.copy(busy = it.busy - port) }
        }
    }

    companion object {
        /** Roles a person may pick for [p]: the server's own list, never WAN. */
        fun roleChoices(p: PortRow): List<String> = p.roleOptions.filter { it != PortRole.WAN }
    }
}
