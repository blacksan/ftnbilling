package ke.co.ftw.ftnadmin.ui.more

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import ke.co.ftw.ftnadmin.data.routers.CreatedRouter
import ke.co.ftw.ftnadmin.data.routers.RouterLiveRepository
import ke.co.ftw.ftnadmin.ui.common.apiResult
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch

data class AddRouterUiState(
    val form: RouterForm = RouterForm(),
    val advanced: Boolean = false,
    /** Server messages per field name (`details.fields` of a 400 `validation_failed`). */
    val fieldErrors: Map<String, String> = emptyMap(),
    val busy: Boolean = false,
    val error: String? = null,
    /** The created router and its one-time join line. Held in memory only. */
    val created: CreatedRouter? = null,
) {
    val canSubmit: Boolean get() = !busy && created == null && form.name.isNotBlank()
}

/** Add router (owner): `POST /routers`, then show the one-time bootstrap line with copy. */
class AddRouterViewModel(
    private val repo: RouterLiveRepository,
    private val onCreated: () -> Unit = {},
) : ViewModel() {
    private val _state = MutableStateFlow(AddRouterUiState())
    val state: StateFlow<AddRouterUiState> = _state.asStateFlow()

    /** Applies [change] to the form and clears the server message for [field]. */
    fun edit(field: String, change: (RouterForm) -> RouterForm) =
        _state.update { it.copy(form = change(it.form), fieldErrors = it.fieldErrors - field, error = null) }

    fun toggleAdvanced() = _state.update { it.copy(advanced = !it.advanced) }

    fun submit() {
        val s = _state.value
        if (!s.canSubmit) return
        _state.update { it.copy(busy = true, error = null, fieldErrors = emptyMap()) }
        viewModelScope.launch {
            val r = apiResult { repo.createRouter(s.form.toInput()) }
            r.fold(
                { c ->
                    _state.update { it.copy(busy = false, created = c) }
                    onCreated()
                },
                { e ->
                    val f = e.toFormFailure()
                    _state.update { it.copy(busy = false, error = f.message, fieldErrors = f.fields, advanced = it.advanced || f.fields.keys.any { k -> k != RouterField.NAME }) }
                },
            )
        }
    }

    override fun onCleared() {
        // The join line is a one-time secret: drop it with the screen.
        _state.value = AddRouterUiState()
    }
}
