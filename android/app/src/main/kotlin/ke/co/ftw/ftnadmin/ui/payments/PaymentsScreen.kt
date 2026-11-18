package ke.co.ftw.ftnadmin.ui.payments

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Add
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FilterChip
import androidx.compose.material3.FloatingActionButton
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.pulltorefresh.PullToRefreshBox
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.compose.runtime.snapshotFlow
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import ke.co.ftw.ftnadmin.core.network.UserInfo
import ke.co.ftw.ftnadmin.domain.Payment
import ke.co.ftw.ftnadmin.ui.common.AccountPicker
import ke.co.ftw.ftnadmin.ui.common.AppTopBar
import ke.co.ftw.ftnadmin.ui.common.CenteredLoading
import ke.co.ftw.ftnadmin.ui.common.EmptyState
import ke.co.ftw.ftnadmin.ui.common.ErrorState
import ke.co.ftw.ftnadmin.ui.common.LoadingSpinner
import ke.co.ftw.ftnadmin.ui.common.LocalContainer
import ke.co.ftw.ftnadmin.ui.common.PackagePicker
import ke.co.ftw.ftnadmin.ui.common.StatusChip
import ke.co.ftw.ftnadmin.ui.common.Tone
import ke.co.ftw.ftnadmin.ui.common.formatDateTime
import ke.co.ftw.ftnadmin.ui.common.formatKes
import ke.co.ftw.ftnadmin.ui.common.vmFactory

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun PaymentsScreen(user: UserInfo, onSettings: () -> Unit, initialFilter: PaymentFilter = PaymentFilter.All) {
    val container = LocalContainer.current
    val vm: PaymentsViewModel = viewModel(
        key = "payments-${initialFilter.name}",
        factory = vmFactory { PaymentsViewModel(container.payments, container.accounts, initialFilter) },
    )
    val ui by vm.state.collectAsStateWithLifecycle()
    val listState = rememberLazyListState()
    val snack = remember { SnackbarHostState() }
    LaunchedEffect(ui.info) {
        ui.info?.let {
            snack.showSnackbar(it)
            vm.clearInfo()
        }
    }
    LaunchedEffect(listState, ui.items.size) {
        snapshotFlow { listState.layoutInfo.visibleItemsInfo.lastOrNull()?.index ?: 0 }.collect { if (it >= ui.items.size - 5) vm.loadMore() }
    }

    Scaffold(
        containerColor = MaterialTheme.colorScheme.background,
        topBar = { AppTopBar("Payments", onSettings = onSettings) },
        snackbarHost = { SnackbarHost(snack) },
        floatingActionButton = {
            if (user.canWrite) {
                FloatingActionButton(onClick = vm::openRecord) { Icon(Icons.Default.Add, contentDescription = "Record payment") }
            }
        },
    ) { inner ->
        Column(Modifier.padding(inner).fillMaxSize()) {
            Row(Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 8.dp), horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                PaymentFilter.entries.forEach { f ->
                    FilterChip(selected = ui.filter == f, onClick = { vm.setFilter(f) }, label = { Text(f.label) })
                }
            }
            if (ui.totals.count > 0) {
                Text(
                    "${ui.totals.count} payments, ${formatKes(ui.totals.revenueKes)}",
                    style = MaterialTheme.typography.labelMedium,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.padding(horizontal = 16.dp),
                )
            }
            when {
                ui.loading -> CenteredLoading()
                ui.error != null && ui.items.isEmpty() -> ErrorState(ui.error!!, vm::retry)
                else -> PullToRefreshBox(ui.refreshing, vm::refresh, Modifier.fillMaxSize()) {
                    if (ui.items.isEmpty()) {
                        EmptyState(if (ui.filter == PaymentFilter.Unmatched) "No unmatched payments." else "No payments yet.")
                    } else {
                        LazyColumn(
                            state = listState,
                            modifier = Modifier.fillMaxSize(),
                            contentPadding = androidx.compose.foundation.layout.PaddingValues(16.dp),
                            verticalArrangement = Arrangement.spacedBy(8.dp),
                        ) {
                            items(ui.items, key = { it.key }) { p -> PaymentRow(p, canAssign = user.canWrite && p.isUnmatched) { vm.openAssign(p) } }
                            if (ui.loadingMore) item { Row(Modifier.fillMaxWidth().padding(8.dp), horizontalArrangement = Arrangement.Center) { LoadingSpinner() } }
                        }
                    }
                }
            }
        }
    }

    PaymentDialogs(ui, vm)
}

@Composable
private fun PaymentRow(p: Payment, canAssign: Boolean, onAssign: () -> Unit) {
    Card(modifier = Modifier.fillMaxWidth(), colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceContainer)) {
        Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
            Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.SpaceBetween) {
                Text(formatKes(p.amountKes), style = MaterialTheme.typography.titleMedium, color = MaterialTheme.colorScheme.onSurface)
                StatusChip(
                    if (p.isUnmatched) "Unmatched" else p.status.replaceFirstChar { it.uppercase() }.ifBlank { "Paid" },
                    if (p.isUnmatched) Tone.Warn else if (p.status == "refunded") Tone.Bad else Tone.Good,
                )
            }
            Text(
                listOf(if (p.flat.isNotBlank()) "Flat ${p.flat}" else "", p.name, p.phone, p.method).filter { it.isNotBlank() }.joinToString("  |  "),
                style = MaterialTheme.typography.bodyMedium,
                color = MaterialTheme.colorScheme.onSurface,
            )
            Text(
                listOf(formatDateTime(p.paidAt ?: p.createdAt), p.billRef).filter { it.isNotBlank() }.joinToString("  |  "),
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            if (canAssign) TextButton(onClick = onAssign) { Text("Assign to a flat") }
        }
    }
}

@Composable
private fun PaymentDialogs(ui: PaymentsUiState, vm: PaymentsViewModel) {
    when (val d = ui.dialog) {
        PaymentDialog.None -> Unit
        PaymentDialog.Record -> AlertDialog(
            onDismissRequest = vm::dismiss,
            title = { Text("Record a payment") },
            text = {
                Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                    AccountPicker(
                        selectedId = ui.form.account?.id,
                        onSelect = { a -> vm.editForm { it.copy(account = a) } },
                        search = vm::searchAccounts,
                    )
                    OutlinedTextField(
                        ui.form.amount, { v -> vm.editForm { it.copy(amount = v.filter(Char::isDigit)) } },
                        label = { Text("Amount (KES)") }, singleLine = true,
                        keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Number),
                        modifier = Modifier.fillMaxWidth(),
                    )
                    OutlinedTextField(
                        ui.form.transCode, { v -> vm.editForm { it.copy(transCode = v.uppercase()) } },
                        label = { Text("M-Pesa code (optional)") }, singleLine = true, modifier = Modifier.fillMaxWidth(),
                    )
                    Text("Package to apply (optional)", style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
                    PackagePicker(ui.packages, ui.form.packageId, { id -> vm.editForm { it.copy(packageId = if (it.packageId == id) null else id) } })
                    ui.duplicatePrompt?.let {
                        Text("$it Record it anyway?", color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall)
                    }
                    ui.dialogError?.let { Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall) }
                }
            },
            confirmButton = {
                if (ui.duplicatePrompt != null) {
                    TextButton(onClick = { vm.record(force = true) }, enabled = !ui.busy) { Text("Record anyway") }
                } else {
                    TextButton(onClick = { vm.record() }, enabled = ui.form.valid && !ui.busy) { Text(if (ui.busy) "Saving..." else "Record") }
                }
            },
            dismissButton = { TextButton(onClick = vm::dismiss) { Text("Cancel") } },
        )
        is PaymentDialog.Assign -> AlertDialog(
            onDismissRequest = vm::dismiss,
            title = { Text("Assign ${formatKes(d.payment.amountKes)}") },
            text = {
                Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                    Text(
                        listOf(d.payment.name, d.payment.phone, d.payment.billRef).filter { it.isNotBlank() }.joinToString("  |  "),
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                    AccountPicker(ui.assignAccount?.id, vm::pickAssignAccount, vm::searchAccounts)
                    Text("Package", style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
                    PackagePicker(ui.packages, ui.assignPackageId, vm::pickAssignPackage)
                    ui.dialogError?.let { Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall) }
                }
            },
            confirmButton = {
                TextButton(onClick = vm::assign, enabled = ui.assignAccount != null && ui.assignPackageId != null && !ui.busy) {
                    Text(if (ui.busy) "Saving..." else "Assign")
                }
            },
            dismissButton = { TextButton(onClick = vm::dismiss) { Text("Cancel") } },
        )
    }
}
