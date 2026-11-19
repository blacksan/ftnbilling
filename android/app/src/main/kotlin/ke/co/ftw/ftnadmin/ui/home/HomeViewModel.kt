package ke.co.ftw.ftnadmin.ui.home

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import ke.co.ftw.ftnadmin.data.routers.Overview
import ke.co.ftw.ftnadmin.data.routers.RouterLive
import ke.co.ftw.ftnadmin.data.routers.RouterLiveRepository
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.common.apiResult
import ke.co.ftw.ftnadmin.ui.common.userMessage
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.collectLatest
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch

/** How many poll samples are kept for the CPU / memory / online sparklines. */
const val HISTORY_SIZE = 40

data class HomeUiState(
    /** Selected router, or null for "All routers". */
    val routerId: String? = null,
    val live: Loadable<RouterLive> = Loadable.Loading,
    val overview: Loadable<Overview> = Loadable.Loading,
    val refreshing: Boolean = false,
    /** A refresh failed but older numbers are still on screen. */
    val staleError: String? = null,
    val cpuHistory: List<Double> = emptyList(),
    val memHistory: List<Double> = emptyList(),
    val onlineHistory: List<Double> = emptyList(),
)

/**
 * Home tab: live cards for the selected router, fleet totals for "All routers".
 * Polls every [pollMs] while the screen is resumed ([startPolling] / [stopPolling]).
 */
@OptIn(ExperimentalCoroutinesApi::class)
class HomeViewModel(
    private val repo: RouterLiveRepository,
    selection: StateFlow<String?>,
    private val pollMs: Long = 5_000,
) : ViewModel() {
    private val _state = MutableStateFlow(HomeUiState(routerId = selection.value))
    val state: StateFlow<HomeUiState> = _state.asStateFlow()
    private var pollJob: Job? = null

    init {
        viewModelScope.launch {
            selection.collectLatest { id ->
                _state.value = HomeUiState(routerId = id)
                fetch(id)
            }
        }
    }

    fun retry() {
        val id = _state.value.routerId
        _state.update { it.copy(live = Loadable.Loading, overview = Loadable.Loading, staleError = null) }
        viewModelScope.launch { fetch(id) }
    }

    /** Pull to refresh. */
    fun refresh() {
        _state.update { it.copy(refreshing = true) }
        viewModelScope.launch { fetch(_state.value.routerId) }
    }

    fun startPolling() {
        if (pollJob?.isActive == true) return
        pollJob = viewModelScope.launch {
            while (isActive) {
                delay(pollMs)
                fetch(_state.value.routerId)
            }
        }
    }

    fun stopPolling() {
        pollJob?.cancel()
        pollJob = null
    }

    private suspend fun fetch(id: String?) {
        if (id != null) {
            val r = apiResult { repo.live(id) }
            _state.update { cur ->
                if (cur.routerId != id) return@update cur
                r.fold(
                    { l ->
                        cur.copy(
                            live = Loadable.Ready(l),
                            refreshing = false,
                            staleError = null,
                            cpuHistory = (cur.cpuHistory + l.system.cpuLoadPct).takeLast(HISTORY_SIZE),
                            memHistory = (cur.memHistory + l.system.memPct).takeLast(HISTORY_SIZE),
                            onlineHistory = (cur.onlineHistory + l.flats.online.toDouble()).takeLast(HISTORY_SIZE),
                        )
                    },
                    { e ->
                        if (cur.live is Loadable.Ready) cur.copy(refreshing = false, staleError = e.userMessage())
                        else cur.copy(live = Loadable.Failed(e.userMessage()), refreshing = false)
                    },
                )
            }
        } else {
            val r = apiResult { repo.overview() }
            _state.update { cur ->
                if (cur.routerId != null) return@update cur
                r.fold(
                    { o -> cur.copy(overview = Loadable.Ready(o), refreshing = false, staleError = null) },
                    { e ->
                        if (cur.overview is Loadable.Ready) cur.copy(refreshing = false, staleError = e.userMessage())
                        else cur.copy(overview = Loadable.Failed(e.userMessage()), refreshing = false)
                    },
                )
            }
        }
    }
}
