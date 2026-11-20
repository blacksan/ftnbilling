package ke.co.ftw.ftnadmin.ui.customers

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.Checkbox
import androidx.compose.material3.FilledTonalButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.FilterChip
import ke.co.ftw.ftnadmin.ui.common.LoadingSpinner
import ke.co.ftw.ftnadmin.ui.common.ChipLabel
import androidx.compose.foundation.verticalScroll
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.layout.Column
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import ke.co.ftw.ftnadmin.core.network.MacOwner
import ke.co.ftw.ftnadmin.core.network.UserInfo
import ke.co.ftw.ftnadmin.domain.Account
import ke.co.ftw.ftnadmin.ui.common.ApartmentField
import ke.co.ftw.ftnadmin.ui.common.ApartmentPickerSheet
import ke.co.ftw.ftnadmin.ui.common.AppTopBar
import ke.co.ftw.ftnadmin.ui.common.CenteredLoading
import ke.co.ftw.ftnadmin.ui.common.ErrorState
import ke.co.ftw.ftnadmin.ui.common.LabeledValue
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.common.LocalContainer
import ke.co.ftw.ftnadmin.ui.common.PackagePicker
import ke.co.ftw.ftnadmin.ui.common.SectionCard
import ke.co.ftw.ftnadmin.ui.common.StatusChip
import ke.co.ftw.ftnadmin.ui.common.Tone
import ke.co.ftw.ftnadmin.ui.common.formatAge
import ke.co.ftw.ftnadmin.ui.common.formatBytes
import ke.co.ftw.ftnadmin.ui.common.formatDateTime
import ke.co.ftw.ftnadmin.ui.common.formatExpiry
import ke.co.ftw.ftnadmin.ui.common.formatKes
import ke.co.ftw.ftnadmin.ui.common.vmFactory

@OptIn(ExperimentalLayoutApi::class)
@Composable
fun CustomerDetailScreen(id: String, user: UserInfo, onBack: () -> Unit, onOpenOther: (String) -> Unit) {
    val container = LocalContainer.current
    val vm: CustomerDetailViewModel = viewModel(key = "detail-$id", factory = vmFactory { CustomerDetailViewModel(id, container.accounts, container.apartments) })
    val ui by vm.state.collectAsStateWithLifecycle()
    val snack = remember { SnackbarHostState() }
    LaunchedEffect(ui.info) {
        ui.info?.let {
            snack.showSnackbar(it)
            vm.clearInfo()
        }
    }
    val title = (ui.account as? Loadable.Ready)?.value?.let { it.flat } ?: "Customer"

    Scaffold(
        containerColor = MaterialTheme.colorScheme.background,
        topBar = { AppTopBar(title, onBack = onBack) },
        snackbarHost = { SnackbarHost(snack) },
    ) { inner ->
        when (val a = ui.account) {
            Loadable.Loading -> CenteredLoading(Modifier.padding(inner))
            is Loadable.Failed -> ErrorState(a.message, vm::load, Modifier.padding(inner))
            is Loadable.Ready -> LazyColumn(
                Modifier.padding(inner).fillMaxSize(),
                contentPadding = androidx.compose.foundation.layout.PaddingValues(16.dp),
                verticalArrangement = Arrangement.spacedBy(12.dp),
            ) {
                val acc = a.value
                item { SummaryCard(acc) }
                if (ui.error != null && ui.dialog == DetailDialog.None) {
                    item { Text(ui.error!!, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodyMedium) }
                }
                if (user.canWrite) {
                    item {
                        FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                            Button(onClick = { vm.open(DetailDialog.Extend) }, enabled = !ui.busy) {
                                Text(if (acc.expiresAt == null) "Activate" else "Extend")
                            }
                            FilledTonalButton(onClick = { vm.open(DetailDialog.ChangePackage) }, enabled = !ui.busy) { Text("Change package") }
                            FilledTonalButton(onClick = { vm.open(DetailDialog.EditMac) }, enabled = !ui.busy) { Text("Edit MAC") }
                            FilledTonalButton(onClick = vm::openMove, enabled = !ui.busy) { Text("Move apartment") }
                            OutlinedButton(onClick = { vm.open(DetailDialog.ConfirmSuspend(!acc.suspended)) }, enabled = !ui.busy) {
                                Text(if (acc.suspended) "Unsuspend" else "Suspend")
                            }
                            OutlinedButton(onClick = vm::requestReconcile, enabled = !ui.busy) { Text("Resync router") }
                        }
                    }
                }
                item { DeviceCard(acc) }
                acc.customer?.let { c ->
                    item {
                        SectionCard("Contact") {
                            LabeledValue("Name", c.name.ifBlank { acc.tenant })
                            LabeledValue("Phone", c.phone.ifBlank { acc.phone })
                            if (c.email.isNotBlank()) LabeledValue("Email", c.email)
                        }
                    }
                }
                acc.usage?.let { item { SectionCard("Usage") { LabeledValue("This month", formatBytes(it.monthBytes)) } } }
                if (acc.payments.isNotEmpty()) {
                    item {
                        SectionCard("Recent payments") {
                            acc.payments.forEach { p ->
                                Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.SpaceBetween) {
                                    Text("${formatDateTime(p.paidAt ?: p.createdAt)}  ${p.method}", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                                    Text(formatKes(p.amountKes), style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurface)
                                }
                            }
                        }
                    }
                }
                if (acc.events.isNotEmpty()) {
                    item {
                        SectionCard("Activity") {
                            acc.events.forEach { e ->
                                Text(
                                    "${formatDateTime(e.time)}  ${e.msg}${if (e.actor.isNotBlank()) " (${e.actor})" else ""}",
                                    style = MaterialTheme.typography.bodySmall,
                                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                                )
                            }
                        }
                    }
                }
            }
        }
    }

    DetailDialogs(ui, vm, onOpenOther)
}

@OptIn(ExperimentalLayoutApi::class)
@Composable
private fun SummaryCard(a: Account) {
    SectionCard("${a.unitLabel}${if (a.floor.isNotBlank()) ", floor ${a.floor}" else ""}") {
        FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
            StatusChip(accountStateLabel(a), accountTone(a))
            if (a.macConflict) StatusChip("Not provisioned", Tone.Bad)
        }
        if (a.problem.isNotBlank()) {
            Text(a.problem, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.error)
        }
        LabeledValue("Apartment", a.apartment?.name.orEmpty())
        LabeledValue("House number", a.unitLabel)
        LabeledValue("Router", a.routerLabel)
        LabeledValue("Tenant", a.tenant)
        LabeledValue("Phone", a.phone)
        LabeledValue("Account code", a.code)
        LabeledValue("Package", a.pkg?.let { "${it.name} (${it.speedMbps} Mbps)" }.orEmpty())
        LabeledValue("Expires", formatExpiry(a.expiresAt))
    }
}

@OptIn(ExperimentalLayoutApi::class)
@Composable
private fun DeviceCard(a: Account) {
    SectionCard("ONT and router") {
        LabeledValue("MAC", a.mac)
        LabeledValue("IP", a.ip)
        a.router?.let { r ->
            LabeledValue("Lease", r.leaseStatus)
            LabeledValue("Last seen", formatAge(r.lastSeen))
            FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                StatusChip(if (r.inSync) "In sync" else "Out of sync", if (r.inSync) Tone.Good else Tone.Warn)
                if (r.pendingTask) StatusChip("Update queued", Tone.Neutral)
                if (r.suspendedOnRouter) StatusChip("Walled on router", Tone.Bad)
            }
            if (r.lastError.isNotBlank()) Text(r.lastError, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall)
        }
        if (a.macConflict) {
            Text(
                "This MAC is also on another account, so the router is not configured for this house. Edit the MAC to fix it.",
                color = MaterialTheme.colorScheme.error,
                style = MaterialTheme.typography.bodySmall,
            )
        }
    }
}

@Composable
private fun DetailDialogs(ui: DetailUiState, vm: CustomerDetailViewModel, onOpenOther: (String) -> Unit) {
    val account = (ui.account as? Loadable.Ready)?.value ?: return
    when (val d = ui.dialog) {
        DetailDialog.None -> Unit
        DetailDialog.Extend -> PackageDialog(
            title = if (account.expiresAt == null) "Activate" else "Extend",
            confirm = if (account.expiresAt == null) "Activate" else "Extend",
            ui = ui,
            initial = account.pkg?.id,
            showNote = true,
            showKeepTime = false,
            onDismiss = vm::dismiss,
        ) { pkg, note, _ -> vm.extendTracked(pkg, note) }
        DetailDialog.ChangePackage -> PackageDialog(
            title = "Change package",
            confirm = "Change",
            ui = ui,
            initial = account.pkg?.id,
            showNote = false,
            showKeepTime = true,
            onDismiss = vm::dismiss,
        ) { pkg, _, keep -> vm.changePackage(pkg, keep) }
        DetailDialog.EditMac -> MacDialog(account, ui, vm, onOpenOther)
        DetailDialog.Move -> MoveDialog(account, ui, vm)
        is DetailDialog.ConfirmSuspend -> AlertDialog(
            onDismissRequest = vm::dismiss,
            title = { Text(if (d.suspend) "Suspend ${account.flat}?" else "Unsuspend ${account.flat}?") },
            text = {
                Text(
                    if (d.suspend) "Internet is cut off until you unsuspend. The router is updated within seconds."
                    else "Internet comes back if the plan has not expired.",
                )
            },
            confirmButton = { TextButton(onClick = { vm.setSuspended(d.suspend) }, enabled = !ui.busy) { Text(if (d.suspend) "Suspend" else "Unsuspend") } },
            dismissButton = { TextButton(onClick = vm::dismiss) { Text("Cancel") } },
        )
        is DetailDialog.LowerPlan -> AlertDialog(
            onDismissRequest = vm::dismiss,
            title = { Text("A better plan is running") },
            text = { Text("${d.message}\n\nApply this plan anyway?") },
            confirmButton = { TextButton(onClick = { vm.extend(d.packageId, d.note, force = true) }, enabled = !ui.busy) { Text("Apply anyway") } },
            dismissButton = { TextButton(onClick = vm::dismiss) { Text("Cancel") } },
        )
    }
}

@Composable
private fun PackageDialog(
    title: String,
    confirm: String,
    ui: DetailUiState,
    initial: String?,
    showNote: Boolean,
    showKeepTime: Boolean,
    onDismiss: () -> Unit,
    onConfirm: (packageId: String, note: String?, keepTime: Boolean) -> Unit,
) {
    var selected by remember { mutableStateOf(initial) }
    var note by remember { mutableStateOf("") }
    var keep by remember { mutableStateOf(true) }
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text(title) },
        text = {
            androidx.compose.foundation.layout.Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                PackagePicker(ui.packages, selected, { selected = it })
                if (showKeepTime) {
                    Row(verticalAlignment = Alignment.CenterVertically) {
                        Checkbox(checked = keep, onCheckedChange = { keep = it })
                        Text("Keep the remaining time", color = MaterialTheme.colorScheme.onSurface)
                    }
                }
                if (showNote) {
                    OutlinedTextField(note, { note = it }, label = { Text("Note (optional)") }, singleLine = true, modifier = Modifier.fillMaxWidth())
                }
                ui.error?.let { Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall) }
            }
        },
        confirmButton = {
            TextButton(onClick = { selected?.let { onConfirm(it, note.ifBlank { null }, keep) } }, enabled = selected != null && !ui.busy) {
                Text(if (ui.busy) "Working..." else confirm)
            }
        },
        dismissButton = { TextButton(onClick = onDismiss) { Text("Cancel") } },
    )
}

@Composable
private fun MacDialog(account: Account, ui: DetailUiState, vm: CustomerDetailViewModel, onOpenOther: (String) -> Unit) {
    AlertDialog(
        onDismissRequest = vm::dismiss,
        title = { Text("Edit MAC for ${account.flat}") },
        text = {
            androidx.compose.foundation.layout.Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                Text(
                    "Use the MAC the router sees on the ONT's WAN port. It can differ from the sticker. " +
                        "Check the New devices list for what is actually connected.",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
                OutlinedTextField(
                    value = ui.macInput,
                    onValueChange = vm::onMacInput,
                    label = { Text("MAC address") },
                    placeholder = { Text("AA:BB:CC:DD:EE:FF") },
                    singleLine = true,
                    isError = ui.macFieldError != null || ui.macOwner != null,
                    supportingText = { ui.macFieldError?.let { Text(it) } },
                    keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Ascii),
                    modifier = Modifier.fillMaxWidth(),
                )
                ui.macOwner?.let { MacConflictNotice(it, onOpen = { id -> vm.dismiss(); onOpenOther(id) }) }
                ui.error?.let { Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall) }
            }
        },
        confirmButton = {
            TextButton(onClick = vm::saveMac, enabled = !ui.busy && ui.macFieldError == null && ui.macInput.isNotBlank()) {
                Text(if (ui.busy) "Saving..." else "Save")
            }
        },
        dismissButton = { TextButton(onClick = vm::dismiss) { Text("Cancel") } },
    )
}

/** Shown for a 409 mac_in_use: says exactly which flat and building already own the MAC. */
@Composable
fun MacConflictNotice(owner: MacOwner, onOpen: ((String) -> Unit)? = null) {
    Card(colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.errorContainer, contentColor = MaterialTheme.colorScheme.onErrorContainer)) {
        androidx.compose.foundation.layout.Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
            Text("MAC already in use", style = MaterialTheme.typography.titleSmall)
            Text(macConflictText(owner), style = MaterialTheme.typography.bodyMedium)
            if (onOpen != null && owner.accountId.isNotBlank()) {
                TextButton(onClick = { onOpen(owner.accountId) }) { Text("Open ${owner.flat}") }
            }
        }
    }
}

fun macConflictText(o: MacOwner): String {
    val where = buildString {
        append("house ").append(o.flat.ifBlank { "?" })
        if (o.code.isNotBlank()) append(" (").append(o.code).append(")")
        if (o.siteName.isNotBlank()) append(" on router ").append(o.siteName)
    }
    return "${o.mac.ifBlank { "This MAC" }} is already used by $where. A MAC can belong to one account only. " +
        "Move it off that account first, or check you typed the right address."
}

/** Move a customer to another apartment: pick it, say the unit, and the router when the apartment has several. */
@OptIn(ExperimentalLayoutApi::class)
@Composable
private fun MoveDialog(account: Account, ui: DetailUiState, vm: CustomerDetailViewModel) {
    var aptId by remember { mutableStateOf(account.apartment?.id.orEmpty()) }
    var unit by remember { mutableStateOf(account.unitLabel) }
    var routerId by remember { mutableStateOf<String?>(null) }
    var picking by remember { mutableStateOf(false) }
    val list = (ui.apartments as? Loadable.Ready)?.value.orEmpty()
    val chosen = list.firstOrNull { it.id == aptId }
    // One router: the server picks it. Several: the operator chooses.
    val needsRouter = (chosen?.routers?.size ?: 0) > 1
    AlertDialog(
        onDismissRequest = vm::dismiss,
        title = { Text("Move ${account.unitLabel}") },
        text = {
            Column(Modifier.verticalScroll(rememberScrollState()), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                Text(
                    "The lease and queue are removed from the old router and written on the new one. This is kept in the account history.",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
                when (val l = ui.apartments) {
                    Loadable.Loading -> LoadingSpinner()
                    is Loadable.Failed -> Text(l.message, color = MaterialTheme.colorScheme.error)
                    is Loadable.Ready -> {
                        ApartmentField(selected = chosen, onClick = { picking = true })
                        if (picking) {
                            ApartmentPickerSheet(
                                apartments = l.value,
                                selectedId = aptId,
                                onSelect = { a ->
                                    if (a.id != aptId) routerId = null
                                    aptId = a.id
                                    picking = false
                                },
                                onDismiss = { picking = false },
                            )
                        }
                    }
                }
                OutlinedTextField(unit, { unit = it }, label = { Text("House number") }, singleLine = true, modifier = Modifier.fillMaxWidth())
                if (needsRouter) {
                    Text("Router", style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
                    FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                        chosen?.routers?.forEach { r ->
                            FilterChip(selected = routerId == r.key, onClick = { routerId = r.key }, label = { ChipLabel(r.label) })
                        }
                    }
                }
                if (chosen != null && chosen.routers.isEmpty()) {
                    Text("This apartment has no router yet. Link one on its page first.", color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall)
                }
                ui.error?.let { Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodyMedium) }
            }
        },
        confirmButton = {
            TextButton(
                onClick = { vm.move(aptId, routerId, unit) },
                enabled = !ui.busy && chosen != null && chosen.routers.isNotEmpty() && unit.isNotBlank() && (!needsRouter || routerId != null),
            ) { Text(if (ui.busy) "Moving..." else "Move") }
        },
        dismissButton = { TextButton(onClick = vm::dismiss) { Text("Cancel") } },
    )
}
