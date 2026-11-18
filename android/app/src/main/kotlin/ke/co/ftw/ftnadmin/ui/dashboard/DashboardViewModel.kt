package ke.co.ftw.ftnadmin.ui.dashboard

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import ke.co.ftw.ftnadmin.data.dashboard.DashboardRepository
import ke.co.ftw.ftnadmin.domain.Dashboard
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.common.apiResult
import ke.co.ftw.ftnadmin.ui.common.userMessage
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch

data class DashboardUiState(
    val data: Loadable<Dashboard> = Loadable.Loading,
    val refreshing: Boolean = false,
    /** Set when a refresh failed but older data is still on screen. */
    val staleError: String? = null,
)

class DashboardViewModel(private val repo: DashboardRepository) : ViewModel() {
    private val _state = MutableStateFlow(DashboardUiState())
    val state: StateFlow<DashboardUiState> = _state.asStateFlow()

    init {
        load()
    }

    fun load() = fetch(initial = true)

    fun refresh() = fetch(initial = false)

    private fun fetch(initial: Boolean) {
        _state.update { if (initial && it.data !is Loadable.Ready) it.copy(data = Loadable.Loading) else it.copy(refreshing = true) }
        viewModelScope.launch {
            val r = apiResult { repo.dashboard() }
            _state.update { cur ->
                r.fold(
                    { d -> DashboardUiState(Loadable.Ready(d)) },
                    { e ->
                        if (cur.data is Loadable.Ready) cur.copy(refreshing = false, staleError = e.userMessage())
                        else DashboardUiState(Loadable.Failed(e.userMessage()))
                    },
                )
            }
        }
    }
}
