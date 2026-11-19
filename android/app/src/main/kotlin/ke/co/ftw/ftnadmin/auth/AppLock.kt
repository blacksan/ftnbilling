package ke.co.ftw.ftnadmin.auth

import android.content.Context
import androidx.biometric.BiometricManager
import androidx.biometric.BiometricManager.Authenticators.BIOMETRIC_WEAK
import androidx.biometric.BiometricManager.Authenticators.DEVICE_CREDENTIAL
import androidx.biometric.BiometricPrompt
import androidx.core.content.ContextCompat
import androidx.fragment.app.FragmentActivity
import ke.co.ftw.ftnadmin.core.prefs.Settings
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.filterNotNull
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.launch

enum class LockState { Unknown, Locked, Unlocked }

sealed interface AuthResult {
    data object Success : AuthResult
    data class Failure(val message: String, val canceled: Boolean) : AuthResult
}

/** App lock: biometric or device credential, re-armed after the app has been backgrounded long enough. */
class AppLock(private val settings: Settings, scope: CoroutineScope) {
    private val _state = MutableStateFlow(LockState.Unknown)
    val state: StateFlow<LockState> = _state.asStateFlow()
    private var backgroundedAtMs = 0L

    init {
        scope.launch {
            val s = settings.state.filterNotNull().first()
            _state.value = if (s.biometricEnabled) LockState.Locked else LockState.Unlocked
        }
    }

    fun onBackground(nowMs: Long) {
        backgroundedAtMs = nowMs
    }

    fun onForeground(nowMs: Long) {
        val s = settings.state.value
        val since = backgroundedAtMs
        backgroundedAtMs = 0L
        if (s == null || !s.biometricEnabled || _state.value != LockState.Unlocked || since == 0L) return
        if (nowMs - since >= s.lockTimeoutMin * 60_000L) _state.value = LockState.Locked
    }

    /** Locks immediately if the lock is enabled. */
    fun lock() {
        if (settings.state.value?.biometricEnabled == true) _state.value = LockState.Locked
    }

    fun unlock() {
        _state.value = LockState.Unlocked
    }

    /** Back to the initial state after a wipe. */
    fun reset() {
        _state.value = LockState.Unlocked
    }

    fun canAuthenticate(context: Context): Boolean =
        BiometricManager.from(context).canAuthenticate(AUTHENTICATORS) == BiometricManager.BIOMETRIC_SUCCESS

    /** Shows the system prompt. Does not change [state]; the caller decides what success means. */
    fun prompt(activity: FragmentActivity, title: String, onResult: (AuthResult) -> Unit) {
        val callback = object : BiometricPrompt.AuthenticationCallback() {
            override fun onAuthenticationSucceeded(result: BiometricPrompt.AuthenticationResult) {
                onResult(AuthResult.Success)
            }

            override fun onAuthenticationError(errorCode: Int, errString: CharSequence) {
                val canceled = errorCode == BiometricPrompt.ERROR_USER_CANCELED ||
                    errorCode == BiometricPrompt.ERROR_NEGATIVE_BUTTON ||
                    errorCode == BiometricPrompt.ERROR_CANCELED
                onResult(AuthResult.Failure(errString.toString(), canceled))
            }
        }
        val info = BiometricPrompt.PromptInfo.Builder()
            .setTitle(title)
            .setAllowedAuthenticators(AUTHENTICATORS)
            .build()
        BiometricPrompt(activity, ContextCompat.getMainExecutor(activity), callback).authenticate(info)
    }

    /** Prompts and unlocks on success. */
    fun unlockWithPrompt(activity: FragmentActivity, onResult: (AuthResult) -> Unit) {
        prompt(activity, "Unlock FTN Admin") { r ->
            if (r is AuthResult.Success) unlock()
            onResult(r)
        }
    }

    private companion object {
        const val AUTHENTICATORS = BIOMETRIC_WEAK or DEVICE_CREDENTIAL
    }
}
