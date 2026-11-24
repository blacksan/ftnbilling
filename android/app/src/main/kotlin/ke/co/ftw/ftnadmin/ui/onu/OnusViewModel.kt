package ke.co.ftw.ftnadmin.ui.onu

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import ke.co.ftw.ftnadmin.data.accounts.AccountsRepository
import ke.co.ftw.ftnadmin.data.onu.Onu
import ke.co.ftw.ftnadmin.data.onu.OnuRepository
import ke.co.ftw.ftnadmin.domain.Account
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.common.apiResult
import ke.co.ftw.ftnadmin.ui.common.userMessage
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch

data class OnusUiState(
    val items: Loadable<List<Onu>> = Loadable.Loading,
    /** The ONU being linked to a house (the picker is open). */
    val linking: Onu? = null,
    val busy: Boolean = false,
    val error: String? = null,
    val info: String? = null,
)

/** The ONUs of one router, with link-to-house and unlink. */
class OnusViewModel(
    private val siteId: String,
    private val repo: OnuRepository,
    private val accounts: AccountsRepository,
) : ViewModel() {
    private val _state = MutableStateFlow(OnusUiState())
    val state: StateFlow<OnusUiState> = _state.asStateFlow()

    init {
        load()
    }

    fun load() {
        viewModelScope.launch {
            val r = apiResult { repo.forRouter(siteId) }
            _state.update { cur ->
                r.fold(
                    { l -> cur.copy(items = Loadable.Ready(l)) },
                    { e -> cur.copy(items = Loadable.Failed(e.userMessage())) },
                )
            }
        }
    }

    fun clearInfo() = _state.update { it.copy(info = null, error = null) }

    fun startLink(onu: Onu) = _state.update { it.copy(linking = onu, error = null) }

    fun cancelLink() = _state.update { it.copy(linking = null, error = null) }

    /** Houses of this router that match the typed text (for the picker). */
    suspend fun searchHouses(query: String): List<Account> =
        accounts.list(siteId = siteId, query = query.ifBlank { null }).items

    fun link(account: Account) {
        val onu = _state.value.linking ?: return
        _state.update { it.copy(busy = true, error = null) }
        viewModelScope.launch {
            val r = apiResult { repo.link(onu.id, account.id) }
            _state.update { cur ->
                r.fold(
                    { cur.copy(busy = false, linking = null, info = "Linked to house ${account.unitLabel}") },
                    { e -> cur.copy(busy = false, error = e.userMessage()) },
                )
            }
            if (r.isSuccess) load()
        }
    }

    fun unlink(onu: Onu) {
        _state.update { it.copy(busy = true, error = null) }
        viewModelScope.launch {
            val r = apiResult { repo.unlink(onu.id) }
            _state.update { cur ->
                r.fold(
                    { cur.copy(busy = false, info = "Unlinked. The box is reset to factory settings when it next checks in.") },
                    { e -> cur.copy(busy = false, error = e.userMessage()) },
                )
            }
            if (r.isSuccess) load()
        }
    }

    /** The owner's answer to a check-in from an unexpected place. */
    fun approve(onu: Onu) = decide(onu, "Approved. The ONU is bound to where it checked in from.") { repo.approve(onu.id) }

    fun block(onu: Onu) = decide(onu, "Blocked. Check-ins from that place are ignored.") { repo.block(onu.id) }

    fun unblock(onu: Onu) = decide(onu, "Unblocked.") { repo.unblock(onu.id) }

    private fun decide(onu: Onu, done: String, call: suspend () -> Onu) {
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

    /** Puts an ONU that is not in the register into it, with [mac] chosen from the MACs it reported. */
    fun addToRegister(onu: Onu, mac: String) {
        _state.update { it.copy(busy = true, error = null) }
        viewModelScope.launch {
            val r = apiResult { repo.addToRegister(onu.id, mac) }
            _state.update { cur ->
                r.fold(
                    { cur.copy(busy = false, info = "Added to the register") },
                    { e -> cur.copy(busy = false, error = e.userMessage()) },
                )
            }
            if (r.isSuccess) load()
        }
    }

    /** The one-click answer to a MAC mismatch: the house takes the MAC the ONU reported. */
    fun useReportedMac(onu: Onu, mac: String) {
        if (onu.accountId.isBlank()) return
        _state.update { it.copy(busy = true, error = null) }
        viewModelScope.launch {
            val r = apiResult { repo.useReportedMac(onu.accountId, mac) }
            _state.update { cur ->
                r.fold(
                    { cur.copy(busy = false, info = "The house now uses the MAC the ONU reported") },
                    { e -> cur.copy(busy = false, error = e.userMessage()) },
                )
            }
            if (r.isSuccess) load()
        }
    }
}
