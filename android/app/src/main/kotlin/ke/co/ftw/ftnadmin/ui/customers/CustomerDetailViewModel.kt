package ke.co.ftw.ftnadmin.ui.customers

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import ke.co.ftw.ftnadmin.core.network.ApiError
import ke.co.ftw.ftnadmin.core.network.ErrorCode
import ke.co.ftw.ftnadmin.core.network.MacOwner
import ke.co.ftw.ftnadmin.data.accounts.AccountsRepository
import ke.co.ftw.ftnadmin.data.apartments.ApartmentsRepository
import ke.co.ftw.ftnadmin.domain.Apartment
import ke.co.ftw.ftnadmin.domain.Account
import ke.co.ftw.ftnadmin.domain.MacAddress
import ke.co.ftw.ftnadmin.domain.ServicePackage
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.common.apiResult
import ke.co.ftw.ftnadmin.ui.common.userMessage
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch

sealed interface DetailDialog {
    data object None : DetailDialog

    /** Activate or extend: pick a package. */
    data object Extend : DetailDialog

    data object ChangePackage : DetailDialog

    data object EditMac : DetailDialog

    /** The customer moves house: new apartment, unit and (when it has several) router. */
    data object Move : DetailDialog

    data class ConfirmSuspend(val suspend: Boolean) : DetailDialog

    /** Server said a better plan is still running; ask before forcing. */
    data class LowerPlan(val packageId: String, val note: String?, val message: String) : DetailDialog
}

data class DetailUiState(
    val account: Loadable<Account> = Loadable.Loading,
    val packages: List<ServicePackage> = emptyList(),
    val dialog: DetailDialog = DetailDialog.None,
    val busy: Boolean = false,
    /** Error shown inside the open dialog (or as a banner when none is open). */
    val error: String? = null,
    val info: String? = null,
    val macInput: String = "",
    val macFieldError: String? = null,
    /** Set on a 409 mac_in_use: names the flat and building that own the MAC. */
    val macOwner: MacOwner? = null,
    /** Apartments to move to, loaded when the Move dialog opens. */
    val apartments: Loadable<List<Apartment>> = Loadable.Loading,
)

class CustomerDetailViewModel(
    private val id: String,
    private val repo: AccountsRepository,
    private val apartmentsRepo: ApartmentsRepository? = null,
) : ViewModel() {
    private val _state = MutableStateFlow(DetailUiState())
    val state: StateFlow<DetailUiState> = _state.asStateFlow()

    init {
        load()
    }

    fun load() {
        _state.update { it.copy(account = Loadable.Loading) }
        viewModelScope.launch {
            val r = apiResult { repo.get(id) }
            _state.update { cur ->
                r.fold(
                    { a -> cur.copy(account = Loadable.Ready(a)) },
                    { e -> cur.copy(account = Loadable.Failed(e.userMessage())) },
                )
            }
            r.getOrNull()?.let { loadPackages(it.routerKey) }
        }
    }

    private suspend fun loadPackages(siteId: String) {
        val r = apiResult { repo.packages(siteId.ifBlank { null }) }
        r.getOrNull()?.let { list -> _state.update { it.copy(packages = list.filterNot { p -> p.disabled }) } }
    }

    fun open(d: DetailDialog) = _state.update {
        val a = (it.account as? Loadable.Ready)?.value
        it.copy(
            dialog = d,
            error = null,
            macOwner = null,
            macFieldError = null,
            macInput = if (d == DetailDialog.EditMac) a?.mac.orEmpty() else it.macInput,
        )
    }

    /** Opens the Move dialog and loads the apartments to pick from. */
    fun openMove() {
        open(DetailDialog.Move)
        val repo = apartmentsRepo ?: return
        _state.update { it.copy(apartments = Loadable.Loading) }
        viewModelScope.launch {
            val r = apiResult { repo.list() }
            _state.update { cur -> r.fold({ l -> cur.copy(apartments = Loadable.Ready(l)) }, { e -> cur.copy(apartments = Loadable.Failed(e.userMessage())) }) }
        }
    }

    /** Re-provisioning happens on the server: the old router loses the lease and queue, the new one gets them. */
    fun move(apartmentId: String, routerId: String?, unit: String) {
        val repo = apartmentsRepo ?: return
        act("Moved. The router change is in the account history.") { repo.moveAccount(id, apartmentId, routerId, unit) }
    }

    fun dismiss() = _state.update { it.copy(dialog = DetailDialog.None, error = null, macOwner = null, macFieldError = null) }

    fun clearInfo() = _state.update { it.copy(info = null, error = null) }

    fun onMacInput(v: String) = _state.update { it.copy(macInput = v, macFieldError = MacAddress.errorFor(v), macOwner = null, error = null) }

    fun setSuspended(suspended: Boolean) = act("Suspended".takeIf { suspended } ?: "Unsuspended") {
        if (suspended) repo.suspendAccount(id) else repo.unsuspendAccount(id)
    }

    fun extend(packageId: String, note: String?, force: Boolean = false) = act("Plan applied") {
        repo.extend(id, packageId, note, force)
    }

    fun changePackage(packageId: String, keepTime: Boolean) = act("Package changed") {
        repo.changePackage(id, packageId, keepTime)
    }

    fun requestReconcile() {
        _state.update { it.copy(busy = true, error = null, info = null) }
        viewModelScope.launch {
            val r = apiResult { repo.reconcile(id) }
            _state.update { it.copy(busy = false, info = if (r.isSuccess) "Router update queued" else null, error = r.exceptionOrNull()?.userMessage()) }
        }
    }

    fun saveMac() {
        val s = _state.value
        val mac = MacAddress.normalize(s.macInput)
        if (mac == null) {
            _state.update { it.copy(macFieldError = MacAddress.check(s.macInput).let { r -> (r as? ke.co.ftw.ftnadmin.domain.MacResult.Invalid)?.problem?.message }) }
            return
        }
        val current = (s.account as? Loadable.Ready)?.value?.mac
        if (mac == current && (s.account as? Loadable.Ready)?.value?.macConflict != true) {
            dismiss()
            return
        }
        act("MAC updated") { repo.changeMac(id, mac) }
    }

    /** Runs a write; on success replaces the account and closes the dialog. */
    private fun act(done: String, block: suspend () -> Account) {
        _state.update { it.copy(busy = true, error = null, info = null, macOwner = null) }
        viewModelScope.launch {
            val r = apiResult(block)
            _state.update { cur ->
                r.fold(
                    { a ->
                        cur.copy(
                            busy = false,
                            account = Loadable.Ready(mergeKeepingExtras(cur.account, a)),
                            dialog = DetailDialog.None,
                            info = done,
                        )
                    },
                    { e -> failed(cur, e) },
                )
            }
        }
    }

    private fun failed(cur: DetailUiState, e: Throwable): DetailUiState {
        val api = e as? ApiError
        return when (api?.code) {
            ErrorCode.MAC_IN_USE -> cur.copy(
                busy = false,
                macOwner = api.macOwner,
                error = if (api.macOwner == null) api.message else null,
            )
            ErrorCode.LOWER_PLAN_ACTIVE -> {
                val d = cur.dialog
                cur.copy(busy = false, dialog = if (d == DetailDialog.Extend) cur.pendingLowerPlan(api) else d, error = null)
            }
            ErrorCode.UNIT_IN_USE -> cur.copy(busy = false, error = "That unit is already used by another customer of the apartment.")
            ErrorCode.VALIDATION_FAILED ->
                cur.copy(busy = false, macFieldError = api.fieldErrors["mac"], error = if (api.fieldErrors["mac"] == null) e.userMessage() else null)
            else -> cur.copy(busy = false, error = e.userMessage())
        }
    }

    private var lastExtend: Pair<String, String?>? = null

    private fun DetailUiState.pendingLowerPlan(e: ApiError): DetailDialog {
        val (pkg, note) = lastExtend ?: return dialog
        return DetailDialog.LowerPlan(pkg, note, e.message)
    }

    fun extendTracked(packageId: String, note: String?) {
        lastExtend = packageId to note
        extend(packageId, note)
    }

    /** Update responses omit customer/payments/events; keep what the screen already has. */
    private fun mergeKeepingExtras(old: Loadable<Account>, new: Account): Account {
        val o = (old as? Loadable.Ready)?.value ?: return new
        return new.copy(
            customer = new.customer ?: o.customer,
            payments = new.payments.ifEmpty { o.payments },
            events = new.events.ifEmpty { o.events },
            usage = new.usage ?: o.usage,
        )
    }
}
