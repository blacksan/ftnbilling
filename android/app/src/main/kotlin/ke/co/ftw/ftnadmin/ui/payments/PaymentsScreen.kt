package ke.co.ftw.ftnadmin.ui.payments

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.BoxWithConstraints
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
import androidx.compose.material3.Button
import androidx.compose.material3.ExtendedFloatingActionButton
import androidx.compose.material3.LocalTextStyle
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FilterChip
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
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
import androidx.compose.foundation.layout.IntrinsicSize
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.height
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.res.pluralStringResource
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.input.KeyboardCapitalization
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.platform.LocalDensity
import ke.co.ftw.ftnadmin.domain.Dashboard
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import ke.co.ftw.ftnadmin.R
import ke.co.ftw.ftnadmin.core.network.UserInfo
import ke.co.ftw.ftnadmin.ui.common.FormSheet
import ke.co.ftw.ftnadmin.ui.common.MonoText
import ke.co.ftw.ftnadmin.ui.common.formatPhone
import ke.co.ftw.ftnadmin.ui.common.houseText
import ke.co.ftw.ftnadmin.ui.common.paymentMethodLabel
import ke.co.ftw.ftnadmin.ui.theme.GeistMono
import ke.co.ftw.ftnadmin.domain.Payment
import ke.co.ftw.ftnadmin.ui.common.AccountPicker
import ke.co.ftw.ftnadmin.ui.common.CardTone
import ke.co.ftw.ftnadmin.ui.common.StatCard
import ke.co.ftw.ftnadmin.ui.common.TabScaffold
import ke.co.ftw.ftnadmin.ui.common.ChipLabel
import ke.co.ftw.ftnadmin.ui.common.ChipRow
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
fun PaymentsScreen(user: UserInfo, onAlerts: () -> Unit, initialFilter: PaymentFilter = PaymentFilter.All) {
    val container = LocalContainer.current
    val vm: PaymentsViewModel = viewModel(
        key = "payments-${initialFilter.name}",
        factory = vmFactory {
            PaymentsViewModel(container.payments, container.accounts, initialFilter, container.dashboard, container.routerContext.selection)
        },
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

    TabScaffold(
        title = stringResource(R.string.tab_payments),
        onAlerts = onAlerts,
        snackbarHost = { SnackbarHost(snack) },
        floatingActionButton = {
            if (user.canWrite) {
                ExtendedFloatingActionButton(
                    onClick = vm::openRecord,
                    icon = { Icon(Icons.Default.Add, contentDescription = null) },
                    text = { Text(stringResource(R.string.record_payment)) },
                    containerColor = MaterialTheme.colorScheme.primaryContainer,
                    contentColor = MaterialTheme.colorScheme.onPrimaryContainer,
                )
            }
        },
    ) { inner ->
        // One scrolling list: the summary cards, filters and totals are its first rows, so at large font sizes they scroll
        // away and the payments stay reachable.
        PullToRefreshBox(ui.refreshing, vm::refresh, Modifier.padding(inner).fillMaxSize()) {
            LazyColumn(
                state = listState,
                modifier = Modifier.fillMaxSize(),
                contentPadding = androidx.compose.foundation.layout.PaddingValues(top = 0.dp, bottom = 96.dp),
                verticalArrangement = Arrangement.spacedBy(8.dp),
            ) {
                item { SummaryCards(ui.summary, onUnmatched = { vm.setFilter(PaymentFilter.Unmatched) }) }
                item {
                    ChipRow {
                        PaymentFilter.entries.forEach { f ->
                            FilterChip(selected = ui.filter == f, onClick = { vm.setFilter(f) }, label = { ChipLabel(stringResource(f.labelRes)) })
                        }
                    }
                }
                if (ui.totals.count > 0) {
                    item {
                        Text(
                            pluralStringResource(R.plurals.payments_total, ui.totals.count, ui.totals.count, formatKes(ui.totals.revenueKes)),
                            style = MaterialTheme.typography.labelMedium,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                            modifier = Modifier.padding(horizontal = 16.dp),
                        )
                    }
                }
                when {
                    ui.loading -> item { Box(Modifier.fillMaxWidth().padding(32.dp), contentAlignment = Alignment.Center) { LoadingSpinner() } }
                    ui.error != null && ui.items.isEmpty() -> item {
                        Column(Modifier.fillMaxWidth().padding(24.dp), horizontalAlignment = Alignment.CenterHorizontally, verticalArrangement = Arrangement.spacedBy(12.dp)) {
                            Text(ui.error!!, style = MaterialTheme.typography.bodyLarge, textAlign = TextAlign.Center)
                            Button(onClick = vm::retry) { Text(stringResource(R.string.action_try_again)) }
                        }
                    }
                    ui.items.isEmpty() -> item {
                        Text(
                            stringResource(if (ui.filter == PaymentFilter.Unmatched) R.string.no_unmatched_payments else R.string.no_payments_yet),
                            style = MaterialTheme.typography.bodyLarge,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                            textAlign = TextAlign.Center,
                            modifier = Modifier.fillMaxWidth().padding(24.dp),
                        )
                    }
                    else -> {
                        items(ui.items, key = { it.key }) { p ->
                            Box(Modifier.padding(horizontal = 16.dp)) { PaymentRow(p, canAssign = user.canWrite && p.isUnmatched) { vm.openAssign(p) } }
                        }
                        if (ui.loadingMore) item { Row(Modifier.fillMaxWidth().padding(8.dp), horizontalArrangement = Arrangement.Center) { LoadingSpinner() } }
                    }
                }
            }
        }
    }

    PaymentDialogs(ui, vm)
}

/** Today / Month / Unmatched as tonal cards. Three in a row only when each card is wide enough for a six-digit amount; otherwise one per row. */
@Composable
private fun SummaryCards(d: Dashboard?, onUnmatched: () -> Unit) {
    if (d == null) return
    val today = d.revenue.todayKes
    val month = d.revenue.monthKes
    val unmatched = d.unmatchedPayments
    val cards: List<@Composable (Modifier) -> Unit> = listOf(
        { m -> StatCard(stringResource(R.string.today), formatKes(today).removePrefix("KES "), m, unit = "KES", tone = CardTone.Feature) },
        { m -> StatCard(stringResource(R.string.this_month), formatKes(month).removePrefix("KES "), m, unit = "KES", tone = CardTone.Good) },
        { m ->
            StatCard(
                stringResource(R.string.unmatched), unmatched.toString(), m,
                tone = if (unmatched > 0) CardTone.Warn else CardTone.Neutral,
                onClick = onUnmatched,
            )
        },
    )
    BoxWithConstraints(Modifier.fillMaxWidth()) {
        if (summaryColumns(maxWidth.value, LocalDensity.current.fontScale) == 1) {
            Column(Modifier.padding(horizontal = 16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                cards.forEach { it(Modifier.fillMaxWidth()) }
            }
        } else {
            Row(
                Modifier.padding(horizontal = 16.dp).height(IntrinsicSize.Min),
                horizontalArrangement = Arrangement.spacedBy(8.dp),
            ) {
                cards.forEach { it(Modifier.weight(1f).fillMaxHeight()) }
            }
        }
    }
}

/** 3 cards in a row when each would be at least 120 dp wide at the default font and the font is not scaled up; else 1. */
internal fun summaryColumns(widthDp: Float, fontScale: Float): Int {
    val each = (widthDp - 32f - 16f) / 3f
    return if (each >= 120f && fontScale < 1.3f) 3 else 1
}

@Composable
private fun PaymentRow(p: Payment, canAssign: Boolean, onAssign: () -> Unit) {
    Card(modifier = Modifier.fillMaxWidth(), colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceContainerHigh)) {
        Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
            Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.spacedBy(8.dp), verticalAlignment = Alignment.Top) {
                Text(formatKes(p.amountKes), style = MaterialTheme.typography.titleMedium, color = MaterialTheme.colorScheme.onSurface, maxLines = 1, modifier = Modifier.weight(1f))
                StatusChip(
                    paymentStatusLabel(p.status),
                    when (p.status) {
                        "matched" -> Tone.Good
                        "unmatched" -> Tone.Warn
                        "refunded" -> Tone.Bad
                        else -> Tone.Neutral
                    },
                    Modifier.weight(1f, fill = false),
                )
            }
            Text(
                listOf(houseText(p.flat), p.name, formatPhone(p.phone), paymentMethodLabel(p.method)).filter { it.isNotBlank() }.joinToString("  |  "),
                style = MaterialTheme.typography.bodyMedium,
                color = MaterialTheme.colorScheme.onSurface,
            )
            Text(formatDateTime(p.paidAt ?: p.createdAt), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            if (p.billRef.isNotBlank()) MonoText(p.billRef, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            if (canAssign) TextButton(onClick = onAssign) { Text(stringResource(R.string.assign_to_house)) }
        }
    }
}

/** "matched" -> "Matched"; an empty status reads "Unknown". */
internal fun paymentStatusLabel(raw: String): String = raw.replace('_', ' ').replaceFirstChar { it.uppercase() }.ifBlank { "Unknown" }

@Composable
private fun PaymentDialogs(ui: PaymentsUiState, vm: PaymentsViewModel) {
    when (val d = ui.dialog) {
        PaymentDialog.None -> Unit
        PaymentDialog.Record -> FormSheet(
            title = stringResource(R.string.record_a_payment),
            onDismiss = vm::dismiss,
            confirmLabel = when {
                ui.duplicatePrompt != null -> stringResource(R.string.record_anyway)
                ui.busy -> stringResource(R.string.saving)
                else -> stringResource(R.string.record)
            },
            onConfirm = { if (ui.duplicatePrompt != null) vm.record(force = true) else vm.record() },
            confirmEnabled = if (ui.duplicatePrompt != null) !ui.busy else ui.form.valid && !ui.busy,
        ) {
            AccountPicker(
                selectedId = ui.form.account?.id,
                onSelect = { a -> vm.editForm { it.copy(account = a) } },
                search = vm::searchAccounts,
            )
            OutlinedTextField(
                ui.form.amount, { v -> vm.editForm { it.copy(amount = v.filter(Char::isDigit)) } },
                label = { Text(stringResource(R.string.amount_kes)) }, singleLine = true,
                keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Number),
                modifier = Modifier.fillMaxWidth(),
            )
            OutlinedTextField(
                ui.form.transCode, { v -> vm.editForm { it.copy(transCode = v.uppercase()) } },
                label = { Text(stringResource(R.string.mpesa_code_optional)) }, singleLine = true,
                textStyle = LocalTextStyle.current.copy(fontFamily = GeistMono),
                keyboardOptions = KeyboardOptions(capitalization = KeyboardCapitalization.Characters),
                modifier = Modifier.fillMaxWidth(),
            )
            Text(stringResource(R.string.plan_to_apply), style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
            PackagePicker(ui.packages, ui.form.packageId, { id -> vm.editForm { it.copy(packageId = if (it.packageId == id) null else id) } })
            ui.duplicatePrompt?.let {
                Text(stringResource(R.string.record_it_anyway, it), color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodyMedium)
            }
            ui.dialogError?.let { Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodyMedium) }
        }
        is PaymentDialog.Assign -> FormSheet(
            title = stringResource(R.string.assign_amount, formatKes(d.payment.amountKes)),
            onDismiss = vm::dismiss,
            confirmLabel = stringResource(if (ui.busy) R.string.saving else R.string.assign),
            onConfirm = vm::assign,
            confirmEnabled = ui.assignAccount != null && ui.assignPackageId != null && !ui.busy,
        ) {
            Text(
                listOf(d.payment.name, formatPhone(d.payment.phone), d.payment.billRef).filter { it.isNotBlank() }.joinToString("  |  "),
                style = MaterialTheme.typography.bodyMedium,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            AccountPicker(ui.assignAccount?.id, vm::pickAssignAccount, vm::searchAccounts)
            Text(stringResource(R.string.label_plan), style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
            PackagePicker(ui.packages, ui.assignPackageId, vm::pickAssignPackage)
            ui.dialogError?.let { Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodyMedium) }
        }
    }
}
