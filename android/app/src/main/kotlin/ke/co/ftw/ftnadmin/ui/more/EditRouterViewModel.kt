package ke.co.ftw.ftnadmin.ui.more

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import ke.co.ftw.ftnadmin.data.routers.RouterLiveRepository
import ke.co.ftw.ftnadmin.domain.CustomerPortStatus
import ke.co.ftw.ftnadmin.data.sites.SitesRepository
import ke.co.ftw.ftnadmin.ui.common.apiResult
import ke.co.ftw.ftnadmin.ui.common.userMessage
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch

data class EditRouterUiState(
    /** Null until the router record has loaded. */
    val form: RouterForm? = null,
    val loadError: String? = null,
    val fieldErrors: Map<String, String> = emptyMap(),
    val busy: Boolean = false,
    val error: String? = null,
    val saved: Boolean = false,
    /** The router's real ports for the pickers (empty until it answers, or when it is not joined). */
    val ports: List<String> = emptyList(),
    /** The server's apply status of the extra customer port. */
    val customerPort: CustomerPortStatus? = null,
    /** The form as it was loaded: a change of the OLT port or VLAN against it needs a confirm. */
    val original: RouterForm? = null,
    /** The confirm dialog for an OLT port or VLAN change is open. */
    val confirmOlt: Boolean = false,
    /** The router's automatic fixes are paused (the confirm dialog says a change is still applied). */
    val autoFixesPaused: Boolean = false,
) {
    val canSave: Boolean get() = !busy && !saved && form != null && form.name.isNotBlank()
}

/** Edit router (owner): loads `GET /routers/{id}`, saves with `PATCH /routers/{id}`. */
class EditRouterViewModel(
    private val routerId: String,
    private val sites: SitesRepository,
    private val repo: RouterLiveRepository,
    private val onSaved: () -> Unit = {},
) : ViewModel() {
    private val _state = MutableStateFlow(EditRouterUiState())
    val state: StateFlow<EditRouterUiState> = _state.asStateFlow()

    init {
        load()
    }

    fun load() {
        _state.update { it.copy(loadError = null) }
        viewModelScope.launch {
            val r = apiResult { sites.site(routerId) }
            r.fold(
                { s ->
                    _state.update { it.copy(form = RouterForm.from(s), original = RouterForm.from(s), customerPort = s.customerPort, autoFixesPaused = s.autoFixes?.paused == true) }
                    loadPorts()
                },
                { e -> _state.update { it.copy(loadError = e.userMessage()) } },
            )
        }
    }

    /** Best effort: the picker is an extra, typed entry always works. */
    private fun loadPorts() {
        viewModelScope.launch {
            apiResult { repo.interfaces(routerId) }.fold(
                { list -> _state.update { it.copy(ports = portChoices(list)) } },
                { },
            )
        }
    }

    fun edit(field: String, change: (RouterForm) -> RouterForm) =
        _state.update { cur -> cur.form?.let { cur.copy(form = change(it), fieldErrors = cur.fieldErrors - field, error = null) } ?: cur }

    /** Saves; an OLT port or VLAN change first asks for a confirm (the dialog calls [confirmSave]). */
    fun save() {
        val s = _state.value
        val form = s.form ?: return
        if (!s.canSave) return
        if (s.original?.let { form.changesOlt(it) } == true) {
            _state.update { it.copy(confirmOlt = true) }
            return
        }
        send(form, confirm = false)
    }

    fun confirmSave() {
        val form = _state.value.form ?: return
        _state.update { it.copy(confirmOlt = false) }
        send(form, confirm = true)
    }

    fun cancelConfirm() = _state.update { it.copy(confirmOlt = false) }

    private fun send(form: RouterForm, confirm: Boolean) {
        _state.update { it.copy(busy = true, error = null, fieldErrors = emptyMap()) }
        viewModelScope.launch {
            val r = apiResult { repo.updateRouter(routerId, form.toInput(confirm)) }
            r.fold(
                { _ ->
                    _state.update { it.copy(busy = false, saved = true) }
                    onSaved()
                },
                { e ->
                    val f = e.toFormFailure()
                    _state.update { it.copy(busy = false, error = f.message, fieldErrors = f.fields) }
                },
            )
        }
    }
}
