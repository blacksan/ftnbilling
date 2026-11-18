package ke.co.ftw.ftnadmin.ui.buildings.setup

import android.os.SystemClock
import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import ke.co.ftw.ftnadmin.core.network.UserInfo
import ke.co.ftw.ftnadmin.data.sites.Cutover
import ke.co.ftw.ftnadmin.data.sites.CutoverState
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

/** Typed building names must match exactly (after trimming), so a slip cannot start a cutover. */
fun nameMatches(typed: String, siteName: String): Boolean = siteName.isNotBlank() && typed.trim() == siteName.trim()

enum class CutoverDialog { None, ConfirmAnyway, Rollback }

data class CutoverUiState(
    val cutover: Loadable<Cutover> = Loadable.Loading,
    val siteName: String = "",
    val typedName: String = "",
    val keepUnknown: Boolean = false,
    val busy: Boolean = false,
    val dialog: CutoverDialog = CutoverDialog.None,
    val forceName: String = "",
    /** Last poll failed; the previous state stays on screen and polling continues. */
    val stale: Boolean = false,
    val message: String? = null,
    /** [SystemClock.elapsedRealtime] when [cutover] arrived; the countdown counts down from there. */
    val receivedAtMs: Long = 0,
)

class CutoverViewModel(
    private val siteId: String,
    private val repo: SiteSetupRepository,
    private val sites: SitesRepository,
    private val pollMs: Long = 2_000,
    private val clock: () -> Long = SystemClock::elapsedRealtime,
) : ViewModel() {
    private val _state = MutableStateFlow(CutoverUiState())
    val state: StateFlow<CutoverUiState> = _state.asStateFlow()
    private var pollJob: Job? = null

    /** Bumped by every person-driven action so a poll that was already in flight cannot overwrite its answer. */
    private var actions = 0

    init {
        viewModelScope.launch {
            apiResult { sites.site(siteId) }.getOrNull()?.let { s -> _state.update { it.copy(siteName = s.name) } }
        }
        viewModelScope.launch {
            val r = apiResult { repo.cutover(siteId) }
            _state.update { cur ->
                r.fold(
                    { c -> cur.withCutover(c) },
                    { e -> cur.copy(cutover = Loadable.Failed(e.userMessage())) },
                )
            }
            ensurePolling()
        }
    }

    private fun CutoverUiState.withCutover(c: Cutover) =
        copy(cutover = Loadable.Ready(c), stale = false, receivedAtMs = clock())

    fun retry() {
        _state.update { it.copy(cutover = Loadable.Loading) }
        viewModelScope.launch {
            val r = apiResult { repo.cutover(siteId) }
            _state.update { cur -> r.fold({ c -> cur.withCutover(c) }, { e -> cur.copy(cutover = Loadable.Failed(e.userMessage())) }) }
            ensurePolling()
        }
    }

    /**
     * Polls every [pollMs] while the cutover is running, rolling back or waiting for Confirm, and
     * stops as soon as it is idle, confirmed, rolled back or failed.
     */
    private fun ensurePolling() {
        if (pollJob?.isActive == true || !isLive()) return
        pollJob = viewModelScope.launch {
            while (isActive && isLive()) {
                delay(pollMs)
                val seen = actions
                val r = apiResult { repo.cutover(siteId) }
                if (seen == actions) _state.update { cur -> r.fold({ c -> cur.withCutover(c) }, { cur.copy(stale = true) }) }
            }
        }
    }

    private fun isLive(): Boolean = (_state.value.cutover as? Loadable.Ready)?.value?.state?.let(CutoverState::isLive) == true

    fun setTypedName(v: String) = _state.update { it.copy(typedName = v) }

    fun setKeepUnknown(v: Boolean) = _state.update { it.copy(keepUnknown = v) }

    fun setForceName(v: String) = _state.update { it.copy(forceName = v) }

    fun openDialog(d: CutoverDialog) = _state.update { it.copy(dialog = d, forceName = "") }

    fun closeDialog() = _state.update { it.copy(dialog = CutoverDialog.None, forceName = "") }

    fun clearMessage() = _state.update { it.copy(message = null) }

    fun canStart(): Boolean {
        val s = _state.value
        return !s.busy && nameMatches(s.typedName, s.siteName)
    }

    fun start() {
        val s = _state.value
        if (!canStart()) return
        act { repo.startCutover(siteId, s.typedName.trim(), s.keepUnknown) }
    }

    /** Normal Confirm: only offered while the server says `can_confirm`. */
    fun confirm() {
        if (_state.value.busy) return
        act { repo.confirmCutover(siteId, force = false, confirmName = null) }
    }

    /** "Confirm anyway": the server also requires the owner role and the typed building name. */
    fun confirmAnyway(user: UserInfo) {
        val s = _state.value
        if (s.busy || !user.isOwner || !nameMatches(s.forceName, s.siteName)) return
        closeDialog()
        act { repo.confirmCutover(siteId, force = true, confirmName = s.forceName.trim()) }
    }

    fun rollback() {
        if (_state.value.busy) return
        closeDialog()
        act { repo.rollbackCutover(siteId) }
    }

    private fun act(call: suspend () -> Cutover) {
        actions++
        _state.update { it.copy(busy = true) }
        viewModelScope.launch {
            val r = apiResult(call)
            _state.update { cur ->
                r.fold(
                    { c -> cur.withCutover(c).copy(busy = false) },
                    { e -> cur.copy(busy = false, message = e.userMessage()) },
                )
            }
            ensurePolling()
        }
    }
}
