package ke.co.ftw.ftnadmin.ui.routers.setup

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import ke.co.ftw.ftnadmin.core.network.UserInfo
import ke.co.ftw.ftnadmin.data.sites.JoinCommand
import ke.co.ftw.ftnadmin.data.sites.RouterSetup
import ke.co.ftw.ftnadmin.data.sites.SiteSetupRepository
import ke.co.ftw.ftnadmin.data.sites.SitesRepository
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

/** Typed router names must match exactly (after trimming), so a slip cannot reset or reboot a router. */
fun nameMatches(typed: String, routerName: String): Boolean = routerName.isNotBlank() && typed.trim() == routerName.trim()

/** The one-time bootstrap line. Held in memory only: never persisted, never logged, dropped with the screen. */
data class BootstrapLineState(
    val busy: Boolean = false,
    val command: JoinCommand? = null,
    val error: String? = null,
)

data class RouterSetupUiState(
    val setup: Loadable<RouterSetup> = Loadable.Loading,
    val refreshing: Boolean = false,
    /** Last poll failed; the previous view stays on screen and polling continues. */
    val stale: Boolean = false,
    val routerName: String = "",
    val line: BootstrapLineState = BootstrapLineState(),
    val resetDialog: Boolean = false,
    val typedName: String = "",
    val resetBusy: Boolean = false,
    val resetError: String? = null,
    val message: String? = null,
)

/**
 * Setup card of one router (API v5): the four derived steps with a light poll (every [pollMs]) while they are not all
 * green, the one-line bootstrap command (owner), and the owner-only reset. Nothing here runs a job: the server derives
 * the steps from its own state.
 */
class RouterSetupViewModel(
    private val routerId: String,
    private val repo: SiteSetupRepository,
    private val sites: SitesRepository,
    private val user: UserInfo,
    private val pollMs: Long = 5_000,
) : ViewModel() {
    private val _state = MutableStateFlow(RouterSetupUiState())
    val state: StateFlow<RouterSetupUiState> = _state.asStateFlow()
    private var pollJob: Job? = null
    private var active = true

    /** Bumped by every person-driven action so a poll already in flight cannot overwrite its answer. */
    private var actions = 0

    init {
        viewModelScope.launch {
            apiResult { sites.site(routerId) }.getOrNull()?.let { s -> _state.update { it.copy(routerName = s.label) } }
        }
        load()
    }

    private fun allGreen() = (_state.value.setup as? Loadable.Ready)?.value?.allGreen == true

    private fun load() {
        viewModelScope.launch {
            val r = apiResult { repo.setup(routerId) }
            _state.update { cur ->
                r.fold(
                    { s -> cur.copy(setup = Loadable.Ready(s), stale = false, refreshing = false) },
                    { e -> if (cur.setup is Loadable.Ready) cur.copy(stale = true, refreshing = false) else cur.copy(setup = Loadable.Failed(e.userMessage()), refreshing = false) },
                )
            }
            ensurePolling()
        }
    }

    fun retry() {
        _state.update { it.copy(setup = Loadable.Loading) }
        load()
    }

    /** Pull-to-refresh. */
    fun refresh() {
        _state.update { it.copy(refreshing = true) }
        load()
    }

    /** The screen is visible again: poll while the steps are not all green. */
    fun resume() {
        active = true
        ensurePolling()
    }

    /** The screen is hidden: stop polling. */
    fun pause() {
        active = false
        pollJob?.cancel()
        pollJob = null
    }

    private fun ensurePolling() {
        if (!active || pollJob?.isActive == true || allGreen()) return
        pollJob = viewModelScope.launch {
            while (isActive && active && !allGreen()) {
                delay(pollMs)
                val seen = actions
                val r = apiResult { repo.setup(routerId) }
                if (seen != actions) continue
                _state.update { cur ->
                    r.fold({ s -> cur.copy(setup = Loadable.Ready(s), stale = false) }, { cur.copy(stale = true) })
                }
            }
        }
    }

    // ---- bootstrap line (owner) ----

    /** Mints a new one-time line; the server voids the previous one. */
    fun getLine() {
        if (!user.isOwner || _state.value.line.busy) return
        actions++
        _state.update { it.copy(line = BootstrapLineState(busy = true)) }
        viewModelScope.launch {
            val r = apiResult { repo.joinCommand(routerId) }
            _state.update {
                it.copy(
                    line = r.fold({ c -> BootstrapLineState(command = c) }, { e -> BootstrapLineState(error = e.userMessage()) }),
                )
            }
            refreshQuietly()
        }
    }

    fun hideLine() = _state.update { it.copy(line = BootstrapLineState()) }

    // ---- reset (owner) ----

    fun openReset() {
        if (!user.isOwner) return
        _state.update { it.copy(resetDialog = true, typedName = "", resetError = null) }
    }

    fun closeReset() = _state.update { it.copy(resetDialog = false, typedName = "", resetError = null) }

    fun setTypedName(v: String) = _state.update { it.copy(typedName = v, resetError = null) }

    fun canReset(): Boolean {
        val s = _state.value
        return user.isOwner && !s.resetBusy && nameMatches(s.typedName, s.routerName)
    }

    /** `POST /routers/{id}/actions/reset {confirm_name}`: answers the new line, shown at once. */
    fun reset() {
        if (!canReset()) return
        val typed = _state.value.typedName.trim()
        actions++
        _state.update { it.copy(resetBusy = true, resetError = null) }
        viewModelScope.launch {
            val r = apiResult { repo.reset(routerId, typed) }
            r.fold(
                { res ->
                    _state.update {
                        it.copy(
                            resetBusy = false,
                            resetDialog = false,
                            typedName = "",
                            line = BootstrapLineState(command = res.join),
                            message = "Router reset. Paste the new line on the router.",
                        )
                    }
                    refreshQuietly()
                },
                { e -> _state.update { it.copy(resetBusy = false, resetError = e.userMessage()) } },
            )
        }
    }

    /** Rereads the setup view after an action, then resumes the poll if needed. */
    private suspend fun refreshQuietly() {
        val r = apiResult { repo.setup(routerId) }
        r.getOrNull()?.let { s -> _state.update { it.copy(setup = Loadable.Ready(s), stale = false) } }
        ensurePolling()
    }

    fun clearMessage() = _state.update { it.copy(message = null) }

    override fun onCleared() {
        // The line is a one-time secret: nothing keeps a copy once the screen is gone.
        _state.update { it.copy(line = BootstrapLineState()) }
    }
}
