package ke.co.ftw.ftnadmin.ui.apartments

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import ke.co.ftw.ftnadmin.core.network.ApiError
import ke.co.ftw.ftnadmin.core.network.ErrorCode
import ke.co.ftw.ftnadmin.data.apartments.ApartmentsRepository
import ke.co.ftw.ftnadmin.data.sites.SitesRepository
import ke.co.ftw.ftnadmin.domain.Apartment
import ke.co.ftw.ftnadmin.domain.ApartmentContact
import ke.co.ftw.ftnadmin.domain.ApartmentForm
import ke.co.ftw.ftnadmin.domain.Site
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.common.apiResult
import ke.co.ftw.ftnadmin.ui.common.userMessage
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch

// ---- list ---------------------------------------------------------------------------------------

data class ApartmentsUiState(
    val items: Loadable<List<Apartment>> = Loadable.Loading,
    val query: String = "",
    val refreshing: Boolean = false,
    val message: String? = null,
)

class ApartmentsViewModel(private val repo: ApartmentsRepository, private val debounceMs: Long = 250) : ViewModel() {
    private val _state = MutableStateFlow(ApartmentsUiState())
    val state: StateFlow<ApartmentsUiState> = _state.asStateFlow()
    private var searchJob: Job? = null

    init {
        load(initial = true)
    }

    private fun load(initial: Boolean) {
        if (initial) _state.update { it.copy(items = Loadable.Loading) }
        val q = _state.value.query
        viewModelScope.launch {
            val r = apiResult { repo.list(query = q) }
            _state.update { cur ->
                if (cur.query != q) return@update cur // a newer search is on its way
                r.fold(
                    { l -> cur.copy(items = Loadable.Ready(l), refreshing = false) },
                    { e ->
                        if (cur.items is Loadable.Ready) cur.copy(refreshing = false, message = e.userMessage())
                        else cur.copy(items = Loadable.Failed(e.userMessage()), refreshing = false)
                    },
                )
            }
        }
    }

    fun onQuery(v: String) {
        _state.update { it.copy(query = v) }
        searchJob?.cancel()
        searchJob = viewModelScope.launch {
            delay(debounceMs)
            load(initial = false)
        }
    }

    fun retry() = load(initial = true)

    fun refresh() {
        _state.update { it.copy(refreshing = true) }
        load(initial = false)
    }

    fun clearMessage() = _state.update { it.copy(message = null) }
}

// ---- detail -------------------------------------------------------------------------------------

data class ApartmentDetailUiState(
    val apartment: Loadable<Apartment> = Loadable.Loading,
    /** Every router, to pick one to link. */
    val allRouters: List<Site> = emptyList(),
    val busy: Boolean = false,
    val message: String? = null,
    val confirmUnlink: String? = null,
    val confirmDelete: Boolean = false,
    val deleted: Boolean = false,
) {
    /** Routers that do not serve this apartment yet. */
    val linkable: List<Site>
        get() {
            val a = (apartment as? Loadable.Ready)?.value ?: return emptyList()
            val have = a.routers.map { it.key }.toSet()
            return allRouters.filter { it.key !in have }
        }
}

class ApartmentDetailViewModel(
    private val id: String,
    private val repo: ApartmentsRepository,
    private val sites: SitesRepository,
) : ViewModel() {
    private val _state = MutableStateFlow(ApartmentDetailUiState())
    val state: StateFlow<ApartmentDetailUiState> = _state.asStateFlow()

    init {
        load()
        viewModelScope.launch {
            apiResult { sites.sites() }.getOrNull()?.let { l -> _state.update { it.copy(allRouters = l) } }
        }
    }

    fun load() {
        _state.update { it.copy(apartment = Loadable.Loading) }
        viewModelScope.launch {
            val r = apiResult { repo.get(id) }
            _state.update { cur ->
                r.fold({ a -> cur.copy(apartment = Loadable.Ready(a)) }, { e -> cur.copy(apartment = Loadable.Failed(e.userMessage())) })
            }
        }
    }

    fun clearMessage() = _state.update { it.copy(message = null) }

    fun link(routerId: String) = act("Router linked") { repo.link(id, routerId) }

    fun askUnlink(routerId: String?) = _state.update { it.copy(confirmUnlink = routerId) }

    fun unlink() {
        val r = _state.value.confirmUnlink ?: return
        _state.update { it.copy(confirmUnlink = null) }
        act("Router unlinked") { repo.unlink(id, r) }
    }

    fun askDelete(v: Boolean) = _state.update { it.copy(confirmDelete = v) }

    fun delete() {
        _state.update { it.copy(confirmDelete = false, busy = true) }
        viewModelScope.launch {
            val r = apiResult { repo.delete(id) }
            _state.update { cur ->
                r.fold(
                    { cur.copy(busy = false, deleted = true) },
                    { e -> cur.copy(busy = false, message = failureText(e)) },
                )
            }
        }
    }

    private fun act(done: String, block: suspend () -> Apartment) {
        _state.update { it.copy(busy = true, message = null) }
        viewModelScope.launch {
            val r = apiResult(block)
            _state.update { cur ->
                r.fold(
                    { a -> cur.copy(busy = false, apartment = Loadable.Ready(a), message = done) },
                    { e -> cur.copy(busy = false, message = failureText(e)) },
                )
            }
        }
    }
}

/** Plain words for the refusals the apartment calls can give. */
internal fun failureText(e: Throwable): String {
    val api = e as? ApiError ?: return e.userMessage()
    return when (api.code) {
        ErrorCode.ROUTER_SERVES_ACCOUNTS -> "${api.message} Move those customers to another router or apartment first."
        ErrorCode.APARTMENT_IN_USE -> "This apartment still has customers (open or closed). Move them first."
        ErrorCode.APARTMENT_NAME_IN_USE -> "Another apartment already has this name."
        else -> e.userMessage()
    }
}

// ---- create / edit ------------------------------------------------------------------------------

data class ApartmentFormUiState(
    val loading: Boolean = false,
    val isNew: Boolean = true,
    val form: ApartmentForm = ApartmentForm(),
    val allRouters: List<Site> = emptyList(),
    /** On create: the routers to link. On edit the routers are linked and unlinked from the apartment page. */
    val routerIds: Set<String> = emptySet(),
    val saving: Boolean = false,
    val error: String? = null,
    val fieldErrors: Map<String, String> = emptyMap(),
    val savedId: String? = null,
) {
    val canSave: Boolean get() = !saving && !loading && form.name.isNotBlank()
}

class ApartmentFormViewModel(
    private val id: String?,
    private val repo: ApartmentsRepository,
    private val sites: SitesRepository,
) : ViewModel() {
    private val _state = MutableStateFlow(ApartmentFormUiState(loading = id != null, isNew = id == null))
    val state: StateFlow<ApartmentFormUiState> = _state.asStateFlow()

    init {
        viewModelScope.launch {
            apiResult { sites.sites() }.getOrNull()?.let { l ->
                // One router only: nothing to choose, so a new apartment starts linked to it.
                _state.update { it.copy(allRouters = l, routerIds = if (id == null && l.size == 1) setOf(l.first().key) else it.routerIds) }
            }
        }
        if (id != null) {
            viewModelScope.launch {
                val r = apiResult { repo.get(id) }
                _state.update { cur ->
                    r.fold(
                        { a ->
                            cur.copy(
                                loading = false,
                                form = ApartmentForm(a.name, a.address, a.notes, a.caretaker, a.landlord),
                            )
                        },
                        { e -> cur.copy(loading = false, error = e.userMessage()) },
                    )
                }
            }
        }
    }

    private fun edit(t: (ApartmentForm) -> ApartmentForm) = _state.update { it.copy(form = t(it.form), error = null, fieldErrors = emptyMap()) }

    fun onName(v: String) = edit { it.copy(name = v) }

    fun onAddress(v: String) = edit { it.copy(address = v) }

    fun onNotes(v: String) = edit { it.copy(notes = v) }

    fun onCaretaker(c: ApartmentContact) = edit { it.copy(caretaker = c) }

    fun onLandlord(c: ApartmentContact) = edit { it.copy(landlord = c) }

    fun toggleRouter(key: String) = _state.update { it.copy(routerIds = if (key in it.routerIds) it.routerIds - key else it.routerIds + key) }

    fun save() {
        val s = _state.value
        if (!s.canSave) return
        _state.update { it.copy(saving = true, error = null, fieldErrors = emptyMap()) }
        viewModelScope.launch {
            val r = apiResult {
                if (id == null) repo.create(s.form, s.routerIds.toList()) else repo.update(id, s.form)
            }
            _state.update { cur ->
                r.fold(
                    { a -> cur.copy(saving = false, savedId = a.id) },
                    { e ->
                        val api = e as? ApiError
                        val fields = api?.takeIf { it.code == ErrorCode.VALIDATION_FAILED }?.fieldErrors.orEmpty()
                        cur.copy(saving = false, fieldErrors = fields, error = if (fields.isEmpty()) failureText(e) else "Some fields need fixing.")
                    },
                )
            }
        }
    }
}
