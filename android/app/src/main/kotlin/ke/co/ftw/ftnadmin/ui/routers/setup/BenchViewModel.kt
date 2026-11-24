package ke.co.ftw.ftnadmin.ui.routers.setup

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import ke.co.ftw.ftnadmin.data.accounts.AccountsRepository
import ke.co.ftw.ftnadmin.data.sites.NewDevicesRepository
import ke.co.ftw.ftnadmin.data.sites.SiteSetupRepository
import ke.co.ftw.ftnadmin.data.sites.Step
import ke.co.ftw.ftnadmin.domain.Account
import ke.co.ftw.ftnadmin.domain.MacAddress
import ke.co.ftw.ftnadmin.domain.MacResult
import ke.co.ftw.ftnadmin.domain.NewDevice
import ke.co.ftw.ftnadmin.domain.ServicePackage
import ke.co.ftw.ftnadmin.ui.common.apiResult
import ke.co.ftw.ftnadmin.ui.common.userMessage
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch

private fun validMac(s: String): String? = (MacAddress.check(s) as? MacResult.Valid)?.mac

data class BenchUiState(
    val devices: List<NewDevice> = emptyList(),
    val packages: List<ServicePackage> = emptyList(),
    val mac: String = "",
    val packageId: String? = null,
    val testAccount: Account? = null,
    val suspendSteps: List<Step> = emptyList(),
    val busy: Boolean = false,
    val message: String? = null,
)

/** Bench helpers: make a test flat from a device learned on the bench port, and run the suspend test. */
class BenchViewModel(
    private val siteId: String,
    private val repo: SiteSetupRepository,
    private val devices: NewDevicesRepository,
    private val accounts: AccountsRepository,
) : ViewModel() {
    private val _state = MutableStateFlow(BenchUiState())
    val state: StateFlow<BenchUiState> = _state.asStateFlow()

    init {
        reload()
    }

    fun reload() {
        viewModelScope.launch {
            apiResult { devices.list(siteId) }.getOrNull()?.let { l ->
                _state.update { it.copy(devices = l.sortedByDescending { d -> d.online }) }
            }
            apiResult { accounts.packages(siteId) }.getOrNull()?.let { l ->
                _state.update { it.copy(packages = l.filterNot { p -> p.disabled }) }
            }
        }
    }

    fun setMac(v: String) = _state.update { it.copy(mac = v) }

    fun setPackage(id: String) = _state.update { it.copy(packageId = id) }

    fun macError(): String? {
        val m = _state.value.mac
        if (m.isBlank()) return null
        return (MacAddress.check(m) as? MacResult.Invalid)?.problem?.message
    }

    fun canCreate(): Boolean {
        val s = _state.value
        return !s.busy && s.packageId != null && validMac(s.mac) != null
    }

    fun createTestFlat() {
        val s = _state.value
        val mac = validMac(s.mac) ?: return
        val pkg = s.packageId ?: return
        if (s.busy) return
        _state.update { it.copy(busy = true, suspendSteps = emptyList()) }
        viewModelScope.launch {
            val r = apiResult { repo.testFlat(siteId, mac, pkg) }
            _state.update { cur ->
                r.fold(
                    { a -> cur.copy(busy = false, testAccount = a, message = "Test house ready: ${a.unitLabel.ifBlank { a.code }}.") },
                    { e -> cur.copy(busy = false, message = e.userMessage()) },
                )
            }
        }
    }

    fun runSuspendTest() {
        val acc = _state.value.testAccount ?: return
        if (_state.value.busy) return
        _state.update { it.copy(busy = true, suspendSteps = emptyList()) }
        viewModelScope.launch {
            val r = apiResult { repo.suspendTest(siteId, acc.id) }
            _state.update { cur ->
                r.fold(
                    { t -> cur.copy(busy = false, suspendSteps = t.steps) },
                    { e -> cur.copy(busy = false, message = e.userMessage()) },
                )
            }
        }
    }

    fun clearMessage() = _state.update { it.copy(message = null) }
}
