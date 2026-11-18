package ke.co.ftw.ftnadmin.ui.routers.setup

import android.os.SystemClock
import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import ke.co.ftw.ftnadmin.data.sites.RouterSetup
import ke.co.ftw.ftnadmin.data.sites.SetupState
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

/** Typed router names must match exactly (after trimming), so a slip cannot start a migration or a reboot. */
fun nameMatches(typed: String, routerName: String): Boolean = routerName.isNotBlank() && typed.trim() == routerName.trim()

data class SetupJobUiState(
    val setup: Loadable<RouterSetup> = Loadable.Loading,
    val routerName: String = "",
    val typedName: String = "",
    val keepUnknown: Boolean = false,
    val busy: Boolean = false,
    val rollbackDialog: Boolean = false,
    /** Last poll failed; the previous state stays on screen and polling continues. */
    val stale: Boolean = false,
    val message: String? = null,
    /** [SystemClock.elapsedRealtime] when [setup] arrived; the countdown counts down from there. */
    val receivedAtMs: Long = 0,
)

/**
 * The automatic provisioning job of one router. The server does everything: the app only shows the
 * progress, starts a migration (single button, typed router name) and offers an emergency rollback.
 * There is no Confirm step (release plan amendment A1).
 */
class SetupJobViewModel(
    private val routerId: String,
    private val repo: SiteSetupRepository,
    private val sites: SitesRepository,
    private val pollMs: Long = 2_000,
    private val clock: () -> Long = SystemClock::elapsedRealtime,
) : ViewModel() {
    private val _state = MutableStateFlow(SetupJobUiState())
    val state: StateFlow<SetupJobUiState> = _state.asStateFlow()
    private var pollJob: Job? = null

    /** Bumped by every person-driven action so a poll that was already in flight cannot overwrite its answer. */
    private var actions = 0

    init {
        viewModelScope.launch {
            apiResult { sites.site(routerId) }.getOrNull()?.let { s -> _state.update { it.copy(routerName = s.label) } }
        }
        load()
    }

    private fun RouterSetup.live() = SetupState.isLive(this.state)

    private fun SetupJobUiState.withSetup(s: RouterSetup) = copy(setup = Loadable.Ready(s), stale = false, receivedAtMs = clock())

    private fun load() {
        viewModelScope.launch {
            val r = apiResult { repo.setup(routerId) }
            _state.update { cur -> r.fold({ s -> cur.withSetup(s) }, { e -> cur.copy(setup = Loadable.Failed(e.userMessage())) }) }
            ensurePolling()
        }
    }

    fun retry() {
        _state.update { it.copy(setup = Loadable.Loading) }
        load()
    }

    /** Polls every [pollMs] while the server is working, and stops once it is settled. */
    private fun ensurePolling() {
        if (pollJob?.isActive == true || !isLive()) return
        pollJob = viewModelScope.launch {
            while (isActive && isLive()) {
                delay(pollMs)
                val seen = actions
                val r = apiResult { repo.setup(routerId) }
                if (seen == actions) _state.update { cur -> r.fold({ s -> cur.withSetup(s) }, { cur.copy(stale = true) }) }
            }
        }
    }

    private fun isLive(): Boolean = (_state.value.setup as? Loadable.Ready)?.value?.live() == true

    fun setTypedName(v: String) = _state.update { it.copy(typedName = v) }

    fun setKeepUnknown(v: Boolean) = _state.update { it.copy(keepUnknown = v) }

    fun openRollback() = _state.update { it.copy(rollbackDialog = true) }

    fun closeRollback() = _state.update { it.copy(rollbackDialog = false) }

    fun clearMessage() = _state.update { it.copy(message = null) }

    fun canStart(): Boolean {
        val s = _state.value
        return !s.busy && nameMatches(s.typedName, s.routerName)
    }

    /** "Start migration": the only manual step, and only for a router that is already serving customers. */
    fun startMigration() {
        val s = _state.value
        if (!canStart()) return
        act { repo.migrate(routerId, s.typedName.trim(), s.keepUnknown) }
    }

    /** Runs the job again after a failure or when it is waiting for the person. */
    fun run() {
        if (_state.value.busy) return
        act { repo.runSetup(routerId) }
    }

    /** Emergency only. The router goes back to hotspot mode within about 30 seconds. */
    fun rollbackNow() {
        if (_state.value.busy) return
        closeRollback()
        act { repo.rollback(routerId) }
    }

    private fun act(call: suspend () -> RouterSetup) {
        actions++
        _state.update { it.copy(busy = true) }
        viewModelScope.launch {
            val r = apiResult(call)
            _state.update { cur ->
                r.fold(
                    { s -> cur.withSetup(s).copy(busy = false) },
                    { e -> cur.copy(busy = false, message = e.userMessage()) },
                )
            }
            ensurePolling()
        }
    }
}
