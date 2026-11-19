package ke.co.ftw.ftnadmin.ui.login

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import ke.co.ftw.ftnadmin.core.network.ApiError
import ke.co.ftw.ftnadmin.core.network.ErrorCode
import ke.co.ftw.ftnadmin.data.auth.AuthRepository
import ke.co.ftw.ftnadmin.data.auth.SecondFactor
import ke.co.ftw.ftnadmin.ui.common.apiResult
import ke.co.ftw.ftnadmin.ui.common.userMessage
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch

enum class FactorMode { Totp, Recovery }

data class LoginUiState(
    val username: String = "",
    val password: String = "",
    val totp: String = "",
    val recoveryCode: String = "",
    val setupCode: String = "",
    val mode: FactorMode = FactorMode.Totp,
    val showSetupCode: Boolean = false,
    val busy: Boolean = false,
    val error: String? = null,
    /** True when the server said two-factor must be enrolled on the website first. */
    val enrolRequired: Boolean = false,
) {
    val canSubmit: Boolean
        get() = !busy && username.isNotBlank() && password.isNotEmpty() &&
            when (mode) {
                FactorMode.Totp -> totp.length == TOTP_DIGITS
                FactorMode.Recovery -> recoveryCode.isNotBlank()
            }

    companion object {
        const val TOTP_DIGITS = 6
    }
}

class LoginViewModel(private val auth: AuthRepository, initialNotice: String? = null) : ViewModel() {
    private val _state = MutableStateFlow(LoginUiState(error = initialNotice))
    val state: StateFlow<LoginUiState> = _state.asStateFlow()

    fun onUsername(v: String) = _state.update { it.copy(username = v, error = null) }

    fun onPassword(v: String) = _state.update { it.copy(password = v, error = null) }

    fun onTotp(v: String) = _state.update { it.copy(totp = v.filter(Char::isDigit).take(LoginUiState.TOTP_DIGITS), error = null) }

    fun onRecovery(v: String) = _state.update { it.copy(recoveryCode = v, error = null) }

    fun onSetupCode(v: String) = _state.update { it.copy(setupCode = v, error = null) }

    fun toggleMode() = _state.update {
        it.copy(mode = if (it.mode == FactorMode.Totp) FactorMode.Recovery else FactorMode.Totp, error = null)
    }

    fun toggleSetupCode() = _state.update { it.copy(showSetupCode = !it.showSetupCode) }

    fun submit() {
        val s = _state.value
        if (!s.canSubmit) return
        _state.update { it.copy(busy = true, error = null, enrolRequired = false) }
        viewModelScope.launch {
            val factor = if (s.mode == FactorMode.Totp) {
                SecondFactor(totp = s.totp, setupCode = s.setupCode)
            } else {
                SecondFactor(recoveryCode = s.recoveryCode, setupCode = s.setupCode)
            }
            val r = apiResult { auth.login(s.username, s.password, factor) }
            _state.update { cur ->
                r.fold(
                    // Success flips AuthState; the password must not outlive the form.
                    onSuccess = { cur.copy(busy = false, password = "", totp = "", recoveryCode = "") },
                    onFailure = { e -> failed(cur, e) },
                )
            }
        }
    }

    private fun failed(cur: LoginUiState, e: Throwable): LoginUiState {
        val api = e as? ApiError
        val code = api?.code
        // Too many wrong authenticator codes: only a recovery code works now.
        if (api != null && api.recoveryCodeRequired) {
            return cur.copy(
                busy = false,
                mode = FactorMode.Recovery,
                totp = "",
                error = "Too many wrong authenticator codes. Authenticator codes are blocked for this account; " +
                    "enter one of your saved recovery codes instead.",
            )
        }
        // TOTP back-off: wait, or switch to a recovery code.
        if (api != null && code == ErrorCode.RATE_LIMITED && cur.mode == FactorMode.Totp && cur.totp.length == LoginUiState.TOTP_DIGITS) {
            val wait = api.retryAfterS?.let { "Wait $it seconds" } ?: "Wait a little"
            return cur.copy(busy = false, totp = "", error = "Too many attempts. $wait before trying another code, or use a recovery code.")
        }
        return when (code) {
            ErrorCode.INVALID_CREDENTIALS ->
                cur.copy(busy = false, totp = "", error = "Wrong username or password.")
            ErrorCode.TOTP_INVALID -> cur.copy(
                busy = false,
                totp = "",
                error = if (cur.mode == FactorMode.Totp) {
                    "That code is wrong or has expired. Enter the current 6-digit code."
                } else {
                    "That recovery code is wrong or already used."
                },
            )
            ErrorCode.TOTP_ENROL_REQUIRED -> cur.copy(
                busy = false,
                enrolRequired = true,
                showSetupCode = true,
                error = "Two-factor sign-in is not set up for this account yet. Finish setting it up in a browser at the admin site, then sign in here.",
            )
            else -> cur.copy(busy = false, error = e.userMessage())
        }
    }
}
