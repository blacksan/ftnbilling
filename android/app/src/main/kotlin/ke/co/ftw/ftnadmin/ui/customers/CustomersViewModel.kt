package ke.co.ftw.ftnadmin.ui.customers

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import ke.co.ftw.ftnadmin.data.accounts.AccountsRepository
import ke.co.ftw.ftnadmin.domain.Account
import ke.co.ftw.ftnadmin.ui.common.apiResult
import ke.co.ftw.ftnadmin.ui.common.userMessage
import kotlinx.coroutines.FlowPreview
import kotlinx.coroutines.Job
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.debounce
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.flow.drop
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch

/** Account states the server filters on (`?state=`). Null means all. */
val ACCOUNT_STATES = listOf("active", "expired", "suspended", "new")

data class CustomersUiState(
    val query: String = "",
    val stateFilter: String? = null,
    val items: List<Account> = emptyList(),
    val nextCursor: String? = null,
    val loading: Boolean = true,
    val loadingMore: Boolean = false,
    val refreshing: Boolean = false,
    val error: String? = null,
) {
    val canLoadMore: Boolean get() = nextCursor != null && !loading && !loadingMore
}

@OptIn(FlowPreview::class)
class CustomersViewModel(private val repo: AccountsRepository, private val searchDebounceMs: Long = 300) : ViewModel() {
    private val _state = MutableStateFlow(CustomersUiState())
    val state: StateFlow<CustomersUiState> = _state.asStateFlow()
    private var job: Job? = null

    init {
        reload(initial = true)
        viewModelScope.launch {
            _state.map { it.query }.distinctUntilChanged().drop(1).debounce(searchDebounceMs).collect { reload(initial = true) }
        }
    }

    fun onQuery(q: String) = _state.update { it.copy(query = q) }

    fun onStateFilter(s: String?) {
        if (_state.value.stateFilter == s) return
        _state.update { it.copy(stateFilter = s) }
        reload(initial = true)
    }

    fun refresh() {
        _state.update { it.copy(refreshing = true) }
        reload(initial = false)
    }

    fun retry() = reload(initial = true)

    private fun reload(initial: Boolean) {
        job?.cancel()
        if (initial) _state.update { it.copy(loading = true, error = null, items = emptyList(), nextCursor = null) }
        val s = _state.value
        job = viewModelScope.launch {
            val r = apiResult { repo.list(state = s.stateFilter, query = s.query.trim().ifBlank { null }) }
            _state.update { cur ->
                r.fold(
                    { p -> cur.copy(items = p.items, nextCursor = p.nextCursor, loading = false, refreshing = false, error = null) },
                    { e -> cur.copy(loading = false, refreshing = false, error = e.userMessage()) },
                )
            }
        }
    }

    fun loadMore() {
        val s = _state.value
        val cursor = s.nextCursor ?: return
        if (!s.canLoadMore) return
        _state.update { it.copy(loadingMore = true) }
        job = viewModelScope.launch {
            val r = apiResult { repo.list(state = s.stateFilter, query = s.query.trim().ifBlank { null }, cursor = cursor) }
            _state.update { cur ->
                r.fold(
                    { p -> cur.copy(items = cur.items + p.items, nextCursor = p.nextCursor, loadingMore = false) },
                    { e -> cur.copy(loadingMore = false, error = e.userMessage()) },
                )
            }
        }
    }
}
