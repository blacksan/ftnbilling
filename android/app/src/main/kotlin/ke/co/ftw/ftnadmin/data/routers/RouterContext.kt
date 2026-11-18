package ke.co.ftw.ftnadmin.data.routers

import ke.co.ftw.ftnadmin.core.prefs.Settings
import ke.co.ftw.ftnadmin.data.sites.SitesRepository
import ke.co.ftw.ftnadmin.domain.Site
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.common.apiResult
import ke.co.ftw.ftnadmin.ui.common.userMessage
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharingStarted
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.flow.stateIn
import kotlinx.coroutines.launch

/**
 * The router list and the router chosen in the top-bar switcher, shared by every tab.
 * [selection] is null for "All routers"; it is saved in [Settings] so it survives restarts, and it
 * falls back to null when the saved router no longer exists.
 */
class RouterContext(
    private val sites: SitesRepository,
    private val settings: Settings,
    private val scope: CoroutineScope,
) {
    private val _routers = MutableStateFlow<Loadable<List<Site>>>(Loadable.Loading)
    val routers: StateFlow<Loadable<List<Site>>> = _routers.asStateFlow()

    val selection: StateFlow<String?> = combine(settings.flow.map { it.selectedRouterId.ifBlank { null } }, _routers) { saved, list ->
        val known = (list as? Loadable.Ready)?.value
        if (saved != null && known != null && known.none { it.key == saved }) null else saved
    }.stateIn(scope, SharingStarted.Eagerly, null)

    /** Reloads the router list. Keeps the old list on screen when the reload fails. */
    fun refresh() {
        scope.launch {
            val r = apiResult { sites.sites() }
            _routers.value = r.fold(
                { l -> Loadable.Ready(l.sortedWith(compareBy<Site> { it.online }.thenBy { it.label.lowercase() })) },
                { e -> if (_routers.value is Loadable.Ready) _routers.value else Loadable.Failed(e.userMessage()) },
            )
        }
    }

    fun select(routerId: String?) {
        scope.launch { settings.update { it.copy(selectedRouterId = routerId.orEmpty()) } }
    }

    /** Router currently selected, if it is in the loaded list. */
    fun selectedRouter(): Site? = (routers.value as? Loadable.Ready)?.value?.firstOrNull { it.key == selection.value }
}
