package ke.co.ftw.ftnadmin.ui.settings

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import ke.co.ftw.ftnadmin.data.auth.AuthRepository
import ke.co.ftw.ftnadmin.domain.DeviceSession
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.common.apiResult
import ke.co.ftw.ftnadmin.ui.common.userMessage
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch

data class SettingsUiState(
    val devices: Loadable<List<DeviceSession>> = Loadable.Loading,
    val revoking: String? = null,
    val message: String? = null,
)

class SettingsViewModel(private val auth: AuthRepository) : ViewModel() {
    private val _state = MutableStateFlow(SettingsUiState())
    val state: StateFlow<SettingsUiState> = _state.asStateFlow()

    init {
        loadDevices()
    }

    fun loadDevices() {
        _state.update { it.copy(devices = Loadable.Loading) }
        viewModelScope.launch {
            val r = apiResult { auth.devices() }
            _state.update {
                it.copy(
                    devices = r.fold(
                        { list -> Loadable.Ready(list.sortedWith(compareByDescending<DeviceSession> { d -> d.current }.thenBy { d -> d.revokedAt != null })) },
                        { e -> Loadable.Failed(e.userMessage()) },
                    ),
                )
            }
        }
    }

    fun revoke(id: String) {
        _state.update { it.copy(revoking = id, message = null) }
        viewModelScope.launch {
            val r = apiResult { auth.revokeDevice(id) }
            _state.update { it.copy(revoking = null, message = r.exceptionOrNull()?.userMessage()) }
            if (r.isSuccess) loadDevices()
        }
    }
}
