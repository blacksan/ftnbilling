package ke.co.ftw.ftnadmin.ui.more

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import ke.co.ftw.ftnadmin.data.routers.CreatedRouter
import ke.co.ftw.ftnadmin.data.routers.NewRouter
import ke.co.ftw.ftnadmin.data.routers.RouterLiveRepository
import ke.co.ftw.ftnadmin.ui.common.apiResult
import ke.co.ftw.ftnadmin.ui.common.userMessage
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch

data class AddRouterUiState(
    val name: String = "",
    val serving: Boolean = false,
    val advanced: Boolean = false,
    val wanInterface: String = "",
    val oltInterface: String = "",
    val vlan: String = "",
    val subnet: String = "",
    val wanCap: String = "",
    val supportPhone: String = "",
    val busy: Boolean = false,
    val error: String? = null,
    /** The created router and its one-time join command. Held in memory only. */
    val created: CreatedRouter? = null,
) {
    val canSubmit: Boolean get() = !busy && created == null && name.isNotBlank()
}

/** Add router (owner): `POST /routers`, then show the one-time join command with copy. */
class AddRouterViewModel(
    private val repo: RouterLiveRepository,
    private val onCreated: () -> Unit = {},
) : ViewModel() {
    private val _state = MutableStateFlow(AddRouterUiState())
    val state: StateFlow<AddRouterUiState> = _state.asStateFlow()

    fun onName(v: String) = _state.update { it.copy(name = v, error = null) }

    fun onServing(v: Boolean) = _state.update { it.copy(serving = v) }

    fun toggleAdvanced() = _state.update { it.copy(advanced = !it.advanced) }

    fun onWan(v: String) = _state.update { it.copy(wanInterface = v) }

    fun onOlt(v: String) = _state.update { it.copy(oltInterface = v) }

    fun onVlan(v: String) = _state.update { it.copy(vlan = v.filter(Char::isDigit)) }

    fun onSubnet(v: String) = _state.update { it.copy(subnet = v) }

    fun onWanCap(v: String) = _state.update { it.copy(wanCap = v.filter(Char::isDigit)) }

    fun onSupportPhone(v: String) = _state.update { it.copy(supportPhone = v) }

    fun submit() {
        val s = _state.value
        if (!s.canSubmit) return
        _state.update { it.copy(busy = true, error = null) }
        viewModelScope.launch {
            val r = apiResult {
                repo.createRouter(
                    NewRouter(
                        name = s.name,
                        servingCustomers = s.serving,
                        wanInterface = s.wanInterface,
                        oltInterface = s.oltInterface,
                        vlanId = s.vlan.toIntOrNull(),
                        customerSubnet = s.subnet,
                        wanCapMbps = s.wanCap.toIntOrNull(),
                        supportPhone = s.supportPhone,
                    ),
                )
            }
            r.fold(
                { c ->
                    _state.update { it.copy(busy = false, created = c) }
                    onCreated()
                },
                { e -> _state.update { it.copy(busy = false, error = e.userMessage()) } },
            )
        }
    }

    override fun onCleared() {
        // The join command is a one-time secret: drop it with the screen.
        _state.value = AddRouterUiState()
    }
}
