package ke.co.ftw.ftnadmin.ui.routers

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

data class RoutersUiState(
    val sites: Loadable<List<Site>> = Loadable.Loading,
    val refreshing: Boolean = false,
)

class RoutersViewModel(private val repo: SitesRepository) : ViewModel() {
    private val _state = MutableStateFlow(RoutersUiState())
    val state: StateFlow<RoutersUiState> = _state.asStateFlow()

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
                    { l -> RoutersUiState(Loadable.Ready(l.sortedWith(compareBy<Site> { it.online }.thenBy { it.label.lowercase() }))) },
                    { e -> if (cur.sites is Loadable.Ready) cur.copy(refreshing = false) else RoutersUiState(Loadable.Failed(e.userMessage())) },
                )
            }
        }
    }
}

val QOE_PERIODS = listOf("24h", "7d", "30d")

/** The words for a QoE period chip: "24h" -> "24 hours". */
fun qoePeriodLabel(code: String): String = when (code) {
    "24h" -> "24 hours"
    "7d" -> "7 days"
    "30d" -> "30 days"
    else -> code
}

data class RouterDetailUiState(
    val site: Loadable<Site> = Loadable.Loading,
    val onts: Loadable<List<Ont>> = Loadable.Loading,
    val qoe: Loadable<Qoe> = Loadable.Loading,
    val period: String = "24h",
    val refreshing: Boolean = false,
)

class RouterDetailViewModel(private val id: String, private val repo: SitesRepository) : ViewModel() {
    private val _state = MutableStateFlow(RouterDetailUiState())
    val state: StateFlow<RouterDetailUiState> = _state.asStateFlow()

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
                        { l -> Loadable.Ready(l.sortedWith(compareBy({ o -> o.unitLabel.length }, { o -> o.unitLabel }))) },
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
