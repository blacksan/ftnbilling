package ke.co.ftw.ftnadmin.ui.onu

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import ke.co.ftw.ftnadmin.core.network.ApiError
import ke.co.ftw.ftnadmin.core.network.ErrorCode
import ke.co.ftw.ftnadmin.data.onu.AccountOnu
import ke.co.ftw.ftnadmin.data.onu.OnuRepository
import ke.co.ftw.ftnadmin.data.onu.WifiReveal
import ke.co.ftw.ftnadmin.domain.WifiRules
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.common.apiResult
import ke.co.ftw.ftnadmin.ui.common.userMessage
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch

/** The Change Wi-Fi sheet. The password lives only here, while the sheet is open; it is cleared on close and never shown back from the server. */
data class WifiForm(
    val ssid: String = "",
    val password: String = "",
    val same: Boolean = true,
    val ssid5: String = "",
    val password5: String = "",
    /** Field key (ssid, password, ssid5, password5) to the reason. */
    val errors: Map<String, String> = emptyMap(),
)

data class WifiUiState(
    val onu: Loadable<AccountOnu> = Loadable.Loading,
    val sheet: Boolean = false,
    val form: WifiForm = WifiForm(),
    val confirmReboot: Boolean = false,
    val busy: Boolean = false,
    val error: String? = null,
    val info: String? = null,
    /** The current Wi-Fi password, while the dialog that shows it is open. */
    val reveal: WifiReveal? = null,
)

class WifiViewModel(
    private val accountId: String,
    private val repo: OnuRepository,
) : ViewModel() {
    private val _state = MutableStateFlow(WifiUiState())
    val state: StateFlow<WifiUiState> = _state.asStateFlow()

    init {
        load()
    }

    fun load() {
        viewModelScope.launch {
            val r = apiResult { repo.ofAccount(accountId) }
            _state.update { cur ->
                r.fold(
                    { v -> cur.copy(onu = Loadable.Ready(v)) },
                    { e -> cur.copy(onu = Loadable.Failed(e.userMessage())) },
                )
            }
        }
    }

    /** Reloads the card (the status of the last request); the owner presses it after a change. */
    fun refresh() = load()

    fun clearInfo() = _state.update { it.copy(info = null, error = null) }

    fun openSheet() = _state.update { cur ->
        val first = (cur.onu as? Loadable.Ready)?.value?.onu?.wifi.orEmpty()
        val name2 = first.firstOrNull { it.band == "2.4" }?.ssid.orEmpty()
        val name5 = first.firstOrNull { it.band == "5" }?.ssid.orEmpty()
        val dual = (cur.onu as? Loadable.Ready)?.value?.onu?.has5ghz ?: true
        cur.copy(sheet = true, error = null, form = WifiForm(ssid = name2, ssid5 = name5, same = !dual || name5.isBlank() || name5 == name2))
    }

    /** True unless the ONU reported that it has no 5 GHz radio. */
    private fun has5(): Boolean = (_state.value.onu as? Loadable.Ready)?.value?.onu?.has5ghz ?: true

    /** Shows the current Wi-Fi password of the house (read from the ONU when it answers, else the saved one). */
    fun showPassword() {
        _state.update { it.copy(busy = true, error = null) }
        viewModelScope.launch {
            val r = apiResult { repo.showWifiPassword(accountId) }
            _state.update { cur ->
                r.fold(
                    { v -> cur.copy(busy = false, reveal = v) },
                    { e -> cur.copy(busy = false, error = e.userMessage()) },
                )
            }
        }
    }

    fun closeReveal() = _state.update { it.copy(reveal = null) }

    /** Closing forgets the typed passwords. */
    fun closeSheet() = _state.update { it.copy(sheet = false, form = WifiForm(), error = null) }

    fun onSsid(v: String) = editForm { copy(ssid = v, errors = errors - "ssid") }

    fun onPassword(v: String) = editForm { copy(password = v, errors = errors - "password") }

    fun onSame(v: Boolean) = editForm { copy(same = v, errors = errors - "ssid5" - "password5") }

    fun onSsid5(v: String) = editForm { copy(ssid5 = v, errors = errors - "ssid5") }

    fun onPassword5(v: String) = editForm { copy(password5 = v, errors = errors - "password5") }

    private fun editForm(f: WifiForm.() -> WifiForm) = _state.update { it.copy(form = it.form.f(), error = null) }

    /** Field problems of [f], keyed by field; empty when the form can be sent. */
    fun validate(f: WifiForm): Map<String, String> = buildMap {
        WifiRules.ssidError(f.ssid)?.let { put("ssid", it) }
        WifiRules.passwordError(f.password)?.let { put("password", it) }
        if (!f.same && has5()) {
            WifiRules.ssidError(f.ssid5)?.let { put("ssid5", it) }
            WifiRules.passwordError(f.password5)?.let { put("password5", it) }
        }
    }

    fun submit() {
        val f = _state.value.form
        val problems = validate(f)
        if (problems.isNotEmpty()) {
            _state.update { it.copy(form = it.form.copy(errors = problems)) }
            return
        }
        _state.update { it.copy(busy = true, error = null) }
        viewModelScope.launch {
            val r = apiResult { repo.setWifi(accountId, f.ssid, f.password, f.same || !has5(), f.ssid5, f.password5) }
            r.fold(
                {
                    _state.update { it.copy(busy = false, sheet = false, form = WifiForm(), info = "Wi-Fi change queued. When it shows Accepted, check the Wi-Fi on a phone and press It works.") }
                    load()
                },
                { e ->
                    val api = e as? ApiError
                    _state.update { cur ->
                        if (api?.code == ErrorCode.VALIDATION_FAILED && api.fieldErrors.isNotEmpty()) {
                            cur.copy(busy = false, form = cur.form.copy(errors = cur.form.errors + wifiFieldErrors(api.fieldErrors)))
                        } else {
                            cur.copy(busy = false, error = e.userMessage())
                        }
                    }
                },
            )
        }
    }

    fun askReboot() = _state.update { it.copy(confirmReboot = true, error = null) }

    fun cancelReboot() = _state.update { it.copy(confirmReboot = false) }

    fun reboot() {
        _state.update { it.copy(busy = true, confirmReboot = false, error = null) }
        viewModelScope.launch {
            val r = apiResult { repo.reboot(accountId) }
            _state.update { cur ->
                r.fold(
                    { cur.copy(busy = false, info = "Reboot queued. The house loses internet for a minute or two.") },
                    { e -> cur.copy(busy = false, error = e.userMessage()) },
                )
            }
            if (r.isSuccess) load()
        }
    }

    /** "It works": the owner saw a phone join the new network. */
    fun confirm() = taskAnswer("Thank you. The change is marked as working for this ONU model.") { repo.confirmWifi(accountId) }

    /** "It didn't work": the box said yes but the network did not change; the change is sent again another way. */
    fun reject() = taskAnswer("Noted. The change is sent again another way.") { repo.rejectWifi(accountId) }

    /** Cancels a queued request before it is sent. */
    fun cancelTask(taskId: String) = taskAnswer("Cancelled.") { repo.cancelTask(taskId) }

    private fun <T> taskAnswer(done: String, call: suspend () -> T) {
        _state.update { it.copy(busy = true, error = null) }
        viewModelScope.launch {
            val r = apiResult { call() }
            _state.update { cur ->
                r.fold(
                    { cur.copy(busy = false, info = done) },
                    { e -> cur.copy(busy = false, error = e.userMessage()) },
                )
            }
            if (r.isSuccess) load()
        }
    }

    fun askRefreshOnu() {
        _state.update { it.copy(busy = true, error = null) }
        viewModelScope.launch {
            val r = apiResult { repo.refresh(accountId) }
            _state.update { cur ->
                r.fold(
                    { cur.copy(busy = false, info = "Asked the ONU to report its Wi-Fi.") },
                    { e -> cur.copy(busy = false, error = e.userMessage()) },
                )
            }
            if (r.isSuccess) load()
        }
    }
}

/** The server names its fields ssid and password; the 5 GHz ones are ssid_5g and password_5g. */
internal fun wifiFieldErrors(fields: Map<String, String>): Map<String, String> = fields.mapKeys { (k, _) ->
    when (k) {
        "ssid_5g" -> "ssid5"
        "password_5g" -> "password5"
        else -> k
    }
}
