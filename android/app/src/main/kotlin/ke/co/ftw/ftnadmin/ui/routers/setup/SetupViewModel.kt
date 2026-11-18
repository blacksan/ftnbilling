package ke.co.ftw.ftnadmin.ui.routers.setup

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import ke.co.ftw.ftnadmin.core.network.ApiError
import ke.co.ftw.ftnadmin.core.network.ErrorCode
import ke.co.ftw.ftnadmin.core.network.UserInfo
import ke.co.ftw.ftnadmin.data.sites.CheckFix
import ke.co.ftw.ftnadmin.data.sites.CheckItem
import ke.co.ftw.ftnadmin.data.sites.Checks
import ke.co.ftw.ftnadmin.data.sites.FixKind
import ke.co.ftw.ftnadmin.data.sites.JoinCommand
import ke.co.ftw.ftnadmin.data.sites.SiteSetupRepository
import ke.co.ftw.ftnadmin.data.sites.SitesRepository
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.common.apiResult
import ke.co.ftw.ftnadmin.ui.common.userMessage
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch

/** True when [user] may run [fix]: operators run `operator` fixes, only owners run `owner` fixes. */
fun canRunFix(user: UserInfo, fix: CheckFix?): Boolean = when {
    fix == null -> false
    fix.kind != FixKind.AUTO && fix.kind != FixKind.CONFIRM -> false
    fix.role == "owner" -> user.isOwner
    else -> user.canWrite
}

/** A mode change the server refused with 409 `bad_state`; the person may override it. */
data class ModeConflict(val mode: String, val message: String)

data class JoinCommandState(
    val busy: Boolean = false,
    /** Held in memory only: never persisted, never logged, dropped when the screen closes. */
    val command: JoinCommand? = null,
    val error: String? = null,
)

data class SetupUiState(
    val checks: Loadable<Checks> = Loadable.Loading,
    val refreshing: Boolean = false,
    val mode: String = "",
    val modeBusy: Boolean = false,
    val modeConflict: ModeConflict? = null,
    val fixing: Set<String> = emptySet(),
    /** Check whose confirm dialog is open. */
    val confirmFix: CheckItem? = null,
    val join: JoinCommandState = JoinCommandState(),
    val message: String? = null,
)

class SetupViewModel(
    private val siteId: String,
    private val repo: SiteSetupRepository,
    private val sites: SitesRepository,
) : ViewModel() {
    private val _state = MutableStateFlow(SetupUiState())
    val state: StateFlow<SetupUiState> = _state.asStateFlow()

    init {
        loadChecks(initial = true, fresh = false)
        viewModelScope.launch {
            apiResult { sites.site(siteId) }.getOrNull()?.let { s -> _state.update { it.copy(mode = s.accessMode) } }
        }
    }

    fun retry() = loadChecks(initial = true, fresh = false)

    /** Pull-to-refresh: asks the router again (`?fresh=1`) instead of using the server's 15 s cache. */
    fun refresh() = loadChecks(initial = false, fresh = true)

    private fun loadChecks(initial: Boolean, fresh: Boolean) {
        _state.update { if (initial) it.copy(checks = Loadable.Loading) else it.copy(refreshing = true) }
        viewModelScope.launch {
            val r = apiResult { repo.checks(siteId, fresh) }
            _state.update { cur ->
                r.fold(
                    { c -> cur.copy(checks = Loadable.Ready(c), refreshing = false) },
                    { e ->
                        if (cur.checks is Loadable.Ready) cur.copy(refreshing = false, message = e.userMessage())
                        else cur.copy(checks = Loadable.Failed(e.userMessage()), refreshing = false)
                    },
                )
            }
        }
    }

    // ---- fixes ----

    /** Auto fixes run at once; `confirm` fixes open a dialog and send nothing until the person taps Apply. */
    fun requestFix(item: CheckItem) {
        when (item.fix?.kind) {
            FixKind.AUTO -> runFix(item, confirm = false)
            FixKind.CONFIRM -> _state.update { it.copy(confirmFix = item) }
        }
    }

    fun cancelFix() = _state.update { it.copy(confirmFix = null) }

    fun confirmFix() {
        val item = _state.value.confirmFix ?: return
        _state.update { it.copy(confirmFix = null) }
        runFix(item, confirm = true)
    }

    private fun runFix(item: CheckItem, confirm: Boolean) {
        if (item.id in _state.value.fixing) return
        _state.update { it.copy(fixing = it.fixing + item.id) }
        viewModelScope.launch {
            val r = apiResult { repo.fix(siteId, item.id, confirm) }
            _state.update { cur ->
                val done = cur.copy(fixing = cur.fixing - item.id)
                r.fold(
                    { res ->
                        val checks = (cur.checks as? Loadable.Ready)?.value
                        val replaced = checks?.copy(items = checks.items.map { if (it.id == res.item.id) res.item else it })
                        done.copy(
                            checks = replaced?.let { Loadable.Ready(it) } ?: cur.checks,
                            message = res.applied.takeIf { it.isNotEmpty() }?.joinToString("; ", prefix = "Done: ") ?: "Fix applied.",
                        )
                    },
                    { e -> done.copy(message = e.userMessage()) },
                )
            }
            // The summary and sibling items may have changed; reread without the server cache.
            if (r.isSuccess) loadChecks(initial = false, fresh = true)
        }
    }

    // ---- mode ----

    fun setMode(mode: String, override: Boolean = false) {
        val cur = _state.value
        if (cur.modeBusy || (mode == cur.mode && !override)) return
        _state.update { it.copy(modeBusy = true, modeConflict = null) }
        viewModelScope.launch {
            val r = apiResult { repo.setMode(siteId, mode, override) }
            _state.update { s ->
                r.fold(
                    { site -> s.copy(modeBusy = false, mode = site.accessMode.ifBlank { mode }, message = "Mode changed.") },
                    { e ->
                        if (e is ApiError && e.status == 409 && e.code == ErrorCode.BAD_STATE) {
                            s.copy(modeBusy = false, modeConflict = ModeConflict(mode, e.message))
                        } else {
                            s.copy(modeBusy = false, message = e.userMessage())
                        }
                    },
                )
            }
            if (r.isSuccess) loadChecks(initial = false, fresh = true)
        }
    }

    fun overrideMode() {
        val c = _state.value.modeConflict ?: return
        setMode(c.mode, override = true)
    }

    fun dismissModeConflict() = _state.update { it.copy(modeConflict = null) }

    // ---- join command (owner) ----

    fun getJoinCommand() {
        if (_state.value.join.busy) return
        _state.update { it.copy(join = JoinCommandState(busy = true)) }
        viewModelScope.launch {
            val r = apiResult { repo.joinCommand(siteId) }
            _state.update {
                it.copy(
                    join = r.fold(
                        { c -> JoinCommandState(command = c) },
                        { e -> JoinCommandState(error = e.userMessage()) },
                    ),
                )
            }
        }
    }

    fun clearJoinCommand() = _state.update { it.copy(join = JoinCommandState()) }

    override fun onCleared() {
        // The command is a one-time secret: nothing keeps a copy once the screen is gone.
        _state.update { it.copy(join = JoinCommandState()) }
    }

    fun clearMessage() = _state.update { it.copy(message = null) }
}
