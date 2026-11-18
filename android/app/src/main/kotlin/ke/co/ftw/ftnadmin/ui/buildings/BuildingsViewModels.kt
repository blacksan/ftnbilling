package ke.co.ftw.ftnadmin.ui.buildings

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import ke.co.ftw.ftnadmin.data.sites.SitesRepository
import ke.co.ftw.ftnadmin.domain.Ont
import ke.co.ftw.ftnadmin.domain.Qoe
import ke.co.ftw.ftnadmin.domain.Site
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.common.apiResult
import ke.co.ftw.ftnadmin.ui.common.userMessage
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch

data class BuildingsUiState(
    val sites: Loadable<List<Site>> = Loadable.Loading,
    val refreshing: Boolean = false,
)

class BuildingsViewModel(private val repo: SitesRepository) : ViewModel() {
    private val _state = MutableStateFlow(BuildingsUiState())
    val state: StateFlow<BuildingsUiState> = _state.asStateFlow()

    init {
        load(initial = true)
    }

    fun retry() = load(initial = true)

    fun refresh() = load(initial = false)

    private fun load(initial: Boolean) {
        _state.update { if (initial) it.copy(sites = Loadable.Loading) else it.copy(refreshing = true) }
        viewModelScope.launch {
            val r = apiResult { repo.sites() }
            _state.update { cur ->
                r.fold(
                    { l -> BuildingsUiState(Loadable.Ready(l.sortedWith(compareBy<Site> { it.online }.thenBy { it.name }))) },
                    { e -> if (cur.sites is Loadable.Ready) cur.copy(refreshing = false) else BuildingsUiState(Loadable.Failed(e.userMessage())) },
                )
            }
        }
    }
}

val QOE_PERIODS = listOf("24h", "7d", "30d")

data class BuildingDetailUiState(
    val site: Loadable<Site> = Loadable.Loading,
    val onts: Loadable<List<Ont>> = Loadable.Loading,
    val qoe: Loadable<Qoe> = Loadable.Loading,
    val period: String = "24h",
    val refreshing: Boolean = false,
)

class BuildingDetailViewModel(private val id: String, private val repo: SitesRepository) : ViewModel() {
    private val _state = MutableStateFlow(BuildingDetailUiState())
    val state: StateFlow<BuildingDetailUiState> = _state.asStateFlow()

    init {
        loadAll(initial = true)
    }

    fun retry() = loadAll(initial = true)

    fun refresh() = loadAll(initial = false)

    fun setPeriod(p: String) {
        if (p == _state.value.period) return
        _state.update { it.copy(period = p) }
        loadQoe()
    }

    private fun loadAll(initial: Boolean) {
        if (initial) _state.update { it.copy(site = Loadable.Loading, onts = Loadable.Loading, qoe = Loadable.Loading) }
        else _state.update { it.copy(refreshing = true) }
        viewModelScope.launch {
            val r = apiResult { repo.site(id) }
            _state.update { it.copy(site = r.fold({ s -> Loadable.Ready(s) }, { e -> Loadable.Failed(e.userMessage()) }), refreshing = false) }
        }
        viewModelScope.launch {
            val r = apiResult { repo.onts(id) }
            _state.update {
                it.copy(
                    onts = r.fold(
                        { l -> Loadable.Ready(l.sortedWith(compareBy({ o -> o.flat.length }, { o -> o.flat }))) },
                        { e -> Loadable.Failed(e.userMessage()) },
                    ),
                )
            }
        }
        loadQoe()
    }

    private fun loadQoe() {
        val p = _state.value.period
        _state.update { it.copy(qoe = Loadable.Loading) }
        viewModelScope.launch {
            val r = apiResult { repo.qoe(id, p) }
            _state.update { if (it.period != p) it else it.copy(qoe = r.fold({ q -> Loadable.Ready(q) }, { e -> Loadable.Failed(e.userMessage()) })) }
        }
    }
}
