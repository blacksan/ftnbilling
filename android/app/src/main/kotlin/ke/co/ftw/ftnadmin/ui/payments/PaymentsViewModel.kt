package ke.co.ftw.ftnadmin.ui.payments

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import ke.co.ftw.ftnadmin.core.network.ApiError
import ke.co.ftw.ftnadmin.core.network.ErrorCode
import ke.co.ftw.ftnadmin.data.accounts.AccountsRepository
import ke.co.ftw.ftnadmin.data.payments.PaymentsRepository
import ke.co.ftw.ftnadmin.data.dashboard.DashboardRepository
import ke.co.ftw.ftnadmin.domain.Account
import ke.co.ftw.ftnadmin.domain.Dashboard
import ke.co.ftw.ftnadmin.domain.Payment
import ke.co.ftw.ftnadmin.domain.PaymentTotals
import ke.co.ftw.ftnadmin.domain.ServicePackage
import ke.co.ftw.ftnadmin.ui.common.apiResult
import ke.co.ftw.ftnadmin.ui.common.userMessage
import kotlinx.coroutines.Job
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.flow.drop
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch

enum class PaymentFilter(val label: String, val status: String?) {
    All("All", null),
    Unmatched("Unmatched", "unmatched"),
}

sealed interface PaymentDialog {
    data object None : PaymentDialog

    data object Record : PaymentDialog

    data class Assign(val payment: Payment) : PaymentDialog
}

data class RecordForm(
    val account: Account? = null,
    val amount: String = "",
    val transCode: String = "",
    val packageId: String? = null,
    val note: String = "",
) {
    val amountKes: Long? get() = amount.trim().toLongOrNull()?.takeIf { it > 0 }
    val valid: Boolean get() = account != null && amountKes != null
}

data class PaymentsUiState(
    val filter: PaymentFilter = PaymentFilter.All,
    val items: List<Payment> = emptyList(),
    val totals: PaymentTotals = PaymentTotals(),
    val nextCursor: String? = null,
    val loading: Boolean = true,
    val loadingMore: Boolean = false,
    val refreshing: Boolean = false,
    val error: String? = null,
    val dialog: PaymentDialog = PaymentDialog.None,
    val packages: List<ServicePackage> = emptyList(),
    val form: RecordForm = RecordForm(),
    val assignAccount: Account? = null,
    val assignPackageId: String? = null,
    val busy: Boolean = false,
    val dialogError: String? = null,
    /** Set after a 409 duplicate_payment so the operator can confirm recording it anyway. */
    val duplicatePrompt: String? = null,
    val info: String? = null,
    /** Summary cards (today, month, unmatched); null until the dashboard answers. */
    val summary: Dashboard? = null,
    val routerId: String? = null,
)

class PaymentsViewModel(
    private val payments: PaymentsRepository,
    private val accounts: AccountsRepository,
    initialFilter: PaymentFilter = PaymentFilter.All,
    private val dashboard: DashboardRepository? = null,
    selection: StateFlow<String?> = MutableStateFlow(null),
) : ViewModel() {
    private val _state = MutableStateFlow(PaymentsUiState(filter = initialFilter, routerId = selection.value))
    val state: StateFlow<PaymentsUiState> = _state.asStateFlow()
    private var job: Job? = null

    init {
        reload(initial = true)
        viewModelScope.launch {
            selection.drop(1).distinctUntilChanged().collect { id ->
                _state.update { it.copy(routerId = id) }
                reload(initial = true)
            }
        }
        viewModelScope.launch {
            apiResult { accounts.packages() }.getOrNull()?.let { l -> _state.update { it.copy(packages = l.filterNot { p -> p.disabled }) } }
        }
    }

    fun setFilter(f: PaymentFilter) {
        if (_state.value.filter == f) return
        _state.update { it.copy(filter = f) }
        reload(initial = true)
    }

    fun refresh() {
        _state.update { it.copy(refreshing = true) }
        reload(initial = false)
    }

    fun retry() = reload(initial = true)

    private fun reload(initial: Boolean) {
        job?.cancel()
        if (initial) _state.update { it.copy(loading = true, error = null, items = emptyList(), nextCursor = null) }
        val f = _state.value.filter
        val router = _state.value.routerId
        loadSummary(router)
        job = viewModelScope.launch {
            val r = apiResult { payments.list(siteId = router, status = f.status) }
            _state.update { cur ->
                r.fold(
                    { p -> cur.copy(items = p.items, totals = p.totals, nextCursor = p.nextCursor, loading = false, refreshing = false, error = null) },
                    { e -> cur.copy(loading = false, refreshing = false, error = e.userMessage()) },
                )
            }
        }
    }

    private fun loadSummary(router: String?) {
        val d = dashboard ?: return
        viewModelScope.launch {
            apiResult { d.dashboard(router) }.getOrNull()?.let { sum -> _state.update { it.copy(summary = sum) } }
        }
    }

    fun loadMore() {
        val s = _state.value
        val cursor = s.nextCursor ?: return
        if (s.loading || s.loadingMore) return
        _state.update { it.copy(loadingMore = true) }
        job = viewModelScope.launch {
            val r = apiResult { payments.list(siteId = s.routerId, status = s.filter.status, cursor = cursor) }
            _state.update { cur ->
                r.fold(
                    { p -> cur.copy(items = cur.items + p.items, nextCursor = p.nextCursor, loadingMore = false) },
                    { e -> cur.copy(loadingMore = false, error = e.userMessage()) },
                )
            }
        }
    }

    suspend fun searchAccounts(q: String): List<Account> = accounts.list(query = q.ifBlank { null }, limit = 25).items

    fun openRecord() = _state.update { it.copy(dialog = PaymentDialog.Record, form = RecordForm(), dialogError = null, duplicatePrompt = null) }

    fun openAssign(p: Payment) = _state.update {
        it.copy(dialog = PaymentDialog.Assign(p), assignAccount = null, assignPackageId = null, dialogError = null)
    }

    fun dismiss() = _state.update { it.copy(dialog = PaymentDialog.None, dialogError = null, duplicatePrompt = null) }

    fun clearInfo() = _state.update { it.copy(info = null) }

    fun editForm(t: (RecordForm) -> RecordForm) = _state.update { it.copy(form = t(it.form), dialogError = null, duplicatePrompt = null) }

    fun pickAssignAccount(a: Account) = _state.update { it.copy(assignAccount = a, dialogError = null) }

    fun pickAssignPackage(id: String) = _state.update { it.copy(assignPackageId = id) }

    fun record(force: Boolean = false) {
        val f = _state.value.form
        val account = f.account ?: return
        val amount = f.amountKes ?: return
        _state.update { it.copy(busy = true, dialogError = null) }
        viewModelScope.launch {
            val r = apiResult { payments.record(account.id, amount, f.transCode, f.packageId, f.note, force) }
            _state.update { cur ->
                r.fold(
                    { cur.copy(busy = false, dialog = PaymentDialog.None, info = "Payment recorded") },
                    { e ->
                        if ((e as? ApiError)?.code == ErrorCode.DUPLICATE_PAYMENT) {
                            cur.copy(busy = false, duplicatePrompt = e.message)
                        } else {
                            cur.copy(busy = false, dialogError = e.userMessage())
                        }
                    },
                )
            }
            if (r.isSuccess) reload(initial = false)
        }
    }

    fun assign() {
        val s = _state.value
        val d = s.dialog as? PaymentDialog.Assign ?: return
        val account = s.assignAccount ?: return
        val pkg = s.assignPackageId ?: return
        _state.update { it.copy(busy = true, dialogError = null) }
        viewModelScope.launch {
            val r = apiResult { payments.assign(d.payment.key, account.id, pkg, null) }
            _state.update { cur ->
                r.fold(
                    { cur.copy(busy = false, dialog = PaymentDialog.None, info = "Assigned to flat ${account.flat}") },
                    { e -> cur.copy(busy = false, dialogError = e.userMessage()) },
                )
            }
            if (r.isSuccess) reload(initial = false)
        }
    }
}
