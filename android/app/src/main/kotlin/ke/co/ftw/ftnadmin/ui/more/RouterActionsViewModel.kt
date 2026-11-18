package ke.co.ftw.ftnadmin.ui.more

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import ke.co.ftw.ftnadmin.core.network.ApiError
import ke.co.ftw.ftnadmin.core.network.ErrorCode
import ke.co.ftw.ftnadmin.core.network.UserInfo
import ke.co.ftw.ftnadmin.data.sites.SiteSetupRepository
import ke.co.ftw.ftnadmin.domain.Site
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.common.apiResult
import ke.co.ftw.ftnadmin.ui.common.userMessage
import ke.co.ftw.ftnadmin.ui.routers.setup.nameMatches
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch

data class RouterActionsUiState(
    val routerId: String? = null,
    val routerName: String = "",
    /** Action running now: `recheck`, `resync` or `reboot`. */
    val busy: String? = null,
    val message: String? = null,
    val rebootDialog: Boolean = false,
    val typedName: String = "",
)

/**
 * Router actions of the More tab for the router chosen in the top bar. Reboot is owner only and needs
 * the router's name typed in full; the server also refuses it while a setup job runs (409 bad_state).
 */
class RouterActionsViewModel(
    private val repo: SiteSetupRepository,
    selection: StateFlow<String?>,
    routers: StateFlow<Loadable<List<Site>>>,
    private val user: UserInfo,
) : ViewModel() {
    private val _state = MutableStateFlow(RouterActionsUiState())
    val state: StateFlow<RouterActionsUiState> = _state.asStateFlow()

    init {
        viewModelScope.launch {
            combine(selection, routers) { id, list -> id to (list as? Loadable.Ready)?.value?.firstOrNull { it.key == id } }
                .collect { (id, site) -> _state.update { it.copy(routerId = id, routerName = site?.label.orEmpty()) } }
        }
    }

    fun clearMessage() = _state.update { it.copy(message = null) }

    fun recheck() {
        val id = _state.value.routerId ?: return
        if (!user.canWrite) return
        run("recheck") {
            val c = repo.recheck(id)
            "Checked: ${c.summary.ok} ok, ${c.summary.warn} warnings, ${c.summary.fail} failed."
        }
    }

    fun resync() {
        val id = _state.value.routerId ?: return
        if (!user.canWrite) return
        run("resync") {
            repo.resync(id)
            "Resync queued. The router is updated in the background."
        }
    }

    fun openReboot() {
        if (!user.isOwner || _state.value.routerId == null) return
        _state.update { it.copy(rebootDialog = true, typedName = "") }
    }

    fun closeReboot() = _state.update { it.copy(rebootDialog = false, typedName = "") }

    fun setTypedName(v: String) = _state.update { it.copy(typedName = v) }

    fun canReboot(): Boolean {
        val s = _state.value
        return user.isOwner && s.routerId != null && s.busy == null && nameMatches(s.typedName, s.routerName)
    }

    fun reboot() {
        if (!canReboot()) return
        val s = _state.value
        val id = s.routerId ?: return
        val typed = s.typedName.trim()
        closeReboot()
        run("reboot") {
            repo.reboot(id, typed)
            "Reboot sent. The router is back in about a minute."
        }
    }

    private fun run(key: String, call: suspend () -> String) {
        if (_state.value.busy != null) return
        _state.update { it.copy(busy = key, message = null) }
        viewModelScope.launch {
            val r = apiResult(call)
            _state.update {
                it.copy(
                    busy = null,
                    message = r.fold(
                        { m -> m },
                        { e ->
                            if (e is ApiError && e.status == 409 && e.code == ErrorCode.BAD_STATE) {
                                "A setup job is running on this router. Try again when it has finished."
                            } else {
                                e.userMessage()
                            }
                        },
                    ),
                )
            }
        }
    }
}
