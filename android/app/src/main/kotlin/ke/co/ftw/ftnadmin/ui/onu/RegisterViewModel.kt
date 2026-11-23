package ke.co.ftw.ftnadmin.ui.onu

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import ke.co.ftw.ftnadmin.core.network.ApiError
import ke.co.ftw.ftnadmin.data.onu.OnuRepository
import ke.co.ftw.ftnadmin.data.onu.RegisterItem
import ke.co.ftw.ftnadmin.domain.MacAddress
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.common.apiResult
import ke.co.ftw.ftnadmin.ui.common.userMessage
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch

data class AddOnuForm(
    val serial: String = "",
    val gponSerial: String = "",
    val mac: String = "",
    val model: String = "",
    val errors: Map<String, String> = emptyMap(),
)

data class RegisterUiState(
    val items: Loadable<List<RegisterItem>> = Loadable.Loading,
    val adding: Boolean = false,
    val form: AddOnuForm = AddOnuForm(),
    val busy: Boolean = false,
    val error: String? = null,
    val info: String? = null,
)

/** The ONU register: the serial + WAN MAC pairs of the ONUs the owner supplies. */
class RegisterViewModel(private val repo: OnuRepository) : ViewModel() {
    private val _state = MutableStateFlow(RegisterUiState())
    val state: StateFlow<RegisterUiState> = _state.asStateFlow()

    init {
        load()
    }

    fun load() {
        viewModelScope.launch {
            val r = apiResult { repo.register() }
            _state.update { cur ->
                r.fold(
                    { l -> cur.copy(items = Loadable.Ready(l)) },
                    { e -> cur.copy(items = Loadable.Failed(e.userMessage())) },
                )
            }
        }
    }

    fun clearInfo() = _state.update { it.copy(info = null, error = null) }

    fun openAdd() = _state.update { it.copy(adding = true, form = AddOnuForm(), error = null) }

    fun closeAdd() = _state.update { it.copy(adding = false, form = AddOnuForm(), error = null) }

    fun onSerial(v: String) = edit { copy(serial = v, errors = errors - "serial") }

    fun onGpon(v: String) = edit { copy(gponSerial = v) }

    fun onMac(v: String) = edit { copy(mac = v, errors = errors - "mac") }

    fun onModel(v: String) = edit { copy(model = v) }

    private fun edit(f: AddOnuForm.() -> AddOnuForm) = _state.update { it.copy(form = it.form.f(), error = null) }

    /** Field problems of [f]; empty when it can be sent. */
    fun validate(f: AddOnuForm): Map<String, String> = buildMap {
        if (f.serial.isBlank()) put("serial", "Enter the serial number.")
        if (MacAddress.normalize(f.mac) == null) put("mac", MacAddress.errorFor(f.mac) ?: "Enter the WAN MAC the router sees.")
    }

    fun save() {
        val f = _state.value.form
        val problems = validate(f)
        if (problems.isNotEmpty()) {
            _state.update { it.copy(form = it.form.copy(errors = problems)) }
            return
        }
        val mac = MacAddress.normalize(f.mac) ?: return
        _state.update { it.copy(busy = true, error = null) }
        viewModelScope.launch {
            val r = apiResult { repo.addRegisterItem(f.serial, f.gponSerial, mac, f.model) }
            r.fold(
                {
                    _state.update { it.copy(busy = false, adding = false, form = AddOnuForm(), info = "ONU added to the register") }
                    load()
                },
                { e ->
                    val api = e as? ApiError
                    _state.update { cur ->
                        val fe = api?.fieldErrors.orEmpty()
                        if (fe.isNotEmpty()) cur.copy(busy = false, form = cur.form.copy(errors = cur.form.errors + fe))
                        else cur.copy(busy = false, error = e.userMessage())
                    }
                },
            )
        }
    }
}
