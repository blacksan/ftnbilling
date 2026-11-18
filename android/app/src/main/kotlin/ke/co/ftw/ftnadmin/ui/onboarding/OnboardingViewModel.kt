package ke.co.ftw.ftnadmin.ui.onboarding

import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.setValue
import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import ke.co.ftw.ftnadmin.AppContainer
import ke.co.ftw.ftnadmin.ui.common.importTunnel
import ke.co.ftw.ftnadmin.ui.common.saveLogin
import kotlinx.coroutines.launch

enum class OnboardingStep { Welcome, Import, Vpn, Login, Lock }

class OnboardingViewModel(private val container: AppContainer) : ViewModel() {
    var step by mutableStateOf(OnboardingStep.Welcome)
        private set
    var busy by mutableStateOf(false)
        private set
    var error by mutableStateOf<String?>(null)
        private set
    var lockEnabled by mutableStateOf(false)
        private set

    fun goTo(next: OnboardingStep) {
        error = null
        step = next
    }

    fun importConfig(text: String) {
        viewModelScope.launch {
            busy = true
            error = container.importTunnel(text)
            busy = false
        }
    }

    fun saveLogin(username: String, password: String, onSaved: () -> Unit) {
        viewModelScope.launch {
            busy = true
            error = container.saveLogin(username, password)
            busy = false
            if (error == null) onSaved()
        }
    }

    fun enableLock() {
        viewModelScope.launch {
            container.settings.update { it.copy(biometricEnabled = true) }
            lockEnabled = true
        }
    }

    fun finish() {
        viewModelScope.launch {
            container.settings.update { it.copy(onboardingDone = true) }
            container.acknowledgeWipe()
        }
    }

    fun reportError(message: String?) {
        error = message
    }
}
