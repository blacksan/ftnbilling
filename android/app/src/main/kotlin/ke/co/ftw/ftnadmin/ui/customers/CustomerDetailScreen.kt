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
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.LocalTextStyle
import androidx.compose.material3.FilledTonalButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.FilterChip
import ke.co.ftw.ftnadmin.ui.common.LoadingSpinner
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
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.input.KeyboardCapitalization
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import ke.co.ftw.ftnadmin.R
import ke.co.ftw.ftnadmin.core.network.MacOwner
import ke.co.ftw.ftnadmin.core.network.UserInfo
import ke.co.ftw.ftnadmin.domain.Account
import ke.co.ftw.ftnadmin.ui.common.ApartmentField
import ke.co.ftw.ftnadmin.ui.common.ApartmentPickerSheet
import ke.co.ftw.ftnadmin.ui.common.AppTopBar
import ke.co.ftw.ftnadmin.ui.common.CenteredLoading
import ke.co.ftw.ftnadmin.ui.common.CheckRow
import ke.co.ftw.ftnadmin.ui.common.FormSheet
import ke.co.ftw.ftnadmin.ui.common.MonoText
import ke.co.ftw.ftnadmin.ui.common.SwitchRow
import ke.co.ftw.ftnadmin.ui.common.accountCardTitle
import ke.co.ftw.ftnadmin.ui.common.customerBarTitle
import ke.co.ftw.ftnadmin.ui.common.formatPhone
import ke.co.ftw.ftnadmin.ui.common.leaseLabel
import ke.co.ftw.ftnadmin.ui.common.onlineStateRes
import ke.co.ftw.ftnadmin.ui.common.paymentMethodLabel
import ke.co.ftw.ftnadmin.ui.equipment.onuStatusLabel
import ke.co.ftw.ftnadmin.ui.theme.GeistMono
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
import ke.co.ftw.ftnadmin.ui.onu.WifiCard
import ke.co.ftw.ftnadmin.ui.onu.WifiViewModel

@OptIn(ExperimentalLayoutApi::class)
@Composable
fun CustomerDetailScreen(id: String, user: UserInfo, onBack: () -> Unit, onOpenOther: (String) -> Unit, openEditMac: Boolean = false) {
    val container = LocalContainer.current
    val vm: CustomerDetailViewModel = viewModel(key = "detail-$id", factory = vmFactory { CustomerDetailViewModel(id, container.accounts, container.apartments) })
    val wifiVm: WifiViewModel = viewModel(key = "wifi-$id", factory = vmFactory { WifiViewModel(id, container.onu) })
    val wifiUi by wifiVm.state.collectAsStateWithLifecycle()
    val ui by vm.state.collectAsStateWithLifecycle()
    val snack = remember { SnackbarHostState() }
    LaunchedEffect(wifiUi.info) {
        wifiUi.info?.let {
            snack.showSnackbar(it)
            wifiVm.clearInfo()
        }
    }
    LaunchedEffect(ui.info) {
        ui.info?.let {
            snack.showSnackbar(it)
            vm.clearInfo()
        }
    }
    val title = customerBarTitle((ui.account as? Loadable.Ready)?.value, stringResource(R.string.customer))
    // Arriving from the Network "Change MAC" action: open the MAC dialog once, when the account has loaded.
    var macOpened by rememberSaveable { mutableStateOf(false) }
    val loaded = ui.account is Loadable.Ready
    LaunchedEffect(openEditMac, loaded) {
        if (openEditMac && loaded && !macOpened) {
            macOpened = true
            vm.open(DetailDialog.EditMac)
        }
    }

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
                                Text(stringResource(if (acc.expiresAt == null) R.string.action_activate else R.string.action_extend))
                            }
                            FilledTonalButton(onClick = { vm.open(DetailDialog.ChangePackage) }, enabled = !ui.busy) { Text(stringResource(R.string.change_plan)) }
                            FilledTonalButton(onClick = { vm.open(DetailDialog.EditDetails) }, enabled = !ui.busy) { Text(stringResource(R.string.edit_details)) }
                            FilledTonalButton(onClick = { vm.open(DetailDialog.EditMac) }, enabled = !ui.busy) { Text(stringResource(R.string.edit_mac)) }
                            FilledTonalButton(onClick = vm::openMove, enabled = !ui.busy) { Text(stringResource(R.string.move_house)) }
                            OutlinedButton(
                                onClick = { vm.open(DetailDialog.ConfirmSuspend(!acc.suspended)) },
                                enabled = !ui.busy,
                                colors = ButtonDefaults.outlinedButtonColors(contentColor = MaterialTheme.colorScheme.error),
                            ) {
                                Text(stringResource(if (acc.suspended) R.string.action_unsuspend else R.string.action_suspend))
                            }
                            OutlinedButton(onClick = vm::requestReconcile, enabled = !ui.busy) { Text(stringResource(R.string.resync_router)) }
                        }
                    }
                }
                item { DeviceCard(acc) }
                item { WifiCard(wifiVm, user.canWrite) }
                // Tenant and phone are in the summary; this card only adds what the customer record has that the account does not.
                acc.customer?.let { c ->
                    val extra = customerExtras(acc, c)
                    if (extra.isNotEmpty()) {
                        item {
                            SectionCard(stringResource(R.string.customer_record)) {
                                extra.forEach { (label, value) -> LabeledValue(stringResource(label), value, mono = label == R.string.label_phone) }
                            }
                        }
                    }
                }
                acc.usage?.let { item { SectionCard(stringResource(R.string.usage)) { LabeledValue(stringResource(R.string.this_month), formatBytes(it.monthBytes)) } } }
                if (acc.payments.isNotEmpty()) {
                    item {
                        SectionCard(stringResource(R.string.recent_payments)) {
                            acc.payments.forEach { p ->
                                Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.spacedBy(12.dp), verticalAlignment = Alignment.Top) {
                                    Text(
                                        "${formatDateTime(p.paidAt ?: p.createdAt)}  ${paymentMethodLabel(p.method)}",
                                        style = MaterialTheme.typography.bodySmall,
                                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                                        modifier = Modifier.weight(1f),
                                    )
                                    Text(formatKes(p.amountKes), style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurface, maxLines = 1, softWrap = false)
                                }
                            }
                        }
                    }
                }
                if (acc.events.isNotEmpty()) {
                    item {
                        SectionCard(stringResource(R.string.activity)) {
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

/** What the customer record has that the account row does not already show: its name or phone when they differ, and the email. */
internal fun customerExtras(a: Account, c: ke.co.ftw.ftnadmin.domain.Customer): List<Pair<Int, String>> = buildList {
    if (c.name.isNotBlank() && c.name.trim() != a.tenant.trim()) add(R.string.label_name to c.name)
    if (c.phone.isNotBlank() && c.phone.trim() != a.phone.trim()) add(R.string.label_phone to c.phone)
    if (c.email.isNotBlank()) add(R.string.label_email to c.email)
}

@OptIn(ExperimentalLayoutApi::class)
@Composable
private fun SummaryCard(a: Account) {
    // The house number is the headline (top bar shows the tenant, this card the place), so it is not repeated as a row.
    SectionCard(accountCardTitle(a).ifBlank { stringResource(R.string.customer) }) {
        FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
            StatusChip(accountStateLabel(a), accountTone(a))
            if (a.macConflict) StatusChip(stringResource(R.string.not_provisioned), Tone.Bad)
        }
        if (a.problem.isNotBlank()) {
            Text(a.problem, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.error)
        }
        LabeledValue(stringResource(R.string.label_router), a.routerLabel)
        LabeledValue(stringResource(R.string.label_tenant), a.tenant)
        LabeledValue(stringResource(R.string.label_phone), formatPhone(a.phone), mono = true)
        LabeledValue(stringResource(R.string.label_whatsapp), if (a.waSamePhone) stringResource(R.string.same_as_phone) else formatPhone(a.whatsapp), mono = !a.waSamePhone)
        LabeledValue(stringResource(R.string.label_account_code), a.code, mono = true)
        LabeledValue(stringResource(R.string.label_plan), a.pkg?.let { "${it.name} (${it.speedMbps} Mbps)" }.orEmpty())
        LabeledValue(stringResource(R.string.label_expires), formatExpiry(a.expiresAt))
        if (a.notes.isNotBlank()) LabeledValue(stringResource(R.string.label_notes), a.notes)
    }
}

@OptIn(ExperimentalLayoutApi::class)
@Composable
private fun DeviceCard(a: Account) {
    SectionCard(stringResource(R.string.devices_on_account)) {
        LabeledValue(stringResource(R.string.label_account_mac), a.mac, mono = true)
        LabeledValue(stringResource(R.string.label_vendor), (a.device?.vendor ?: a.macVendor).ifBlank { stringResource(R.string.state_unknown) })
        a.device?.let { d ->
            LabeledValue(stringResource(R.string.label_state), stringResource(onlineStateRes(d.online)))
            LabeledValue(stringResource(R.string.label_last_seen), formatAge(d.lastSeen))
            LabeledValue(stringResource(R.string.label_port), d.port.ifBlank { stringResource(R.string.state_unknown) })
        }
        LabeledValue(stringResource(R.string.label_ip), a.ip, mono = true)
        a.router?.let { r ->
            LabeledValue(stringResource(R.string.label_lease), leaseLabel(r.leaseStatus))
            if (a.device == null) LabeledValue(stringResource(R.string.label_last_seen), formatAge(r.lastSeen))
            FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                StatusChip(stringResource(if (r.inSync) R.string.in_sync else R.string.out_of_sync), if (r.inSync) Tone.Good else Tone.Warn)
                if (r.pendingTask) StatusChip(stringResource(R.string.update_queued), Tone.Neutral)
                if (r.suspendedOnRouter) StatusChip(stringResource(R.string.walled_on_router), Tone.Bad)
            }
            if (r.lastError.isNotBlank()) Text(r.lastError, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall)
        }
        if (a.macConflict) {
            Text(
                stringResource(R.string.mac_conflict_hint),
                color = MaterialTheme.colorScheme.error,
                style = MaterialTheme.typography.bodySmall,
            )
        }
        if (a.equipment.isNotEmpty()) {
            Text(stringResource(R.string.equipment_register), style = MaterialTheme.typography.labelLarge, color = MaterialTheme.colorScheme.onSurfaceVariant)
            a.equipment.forEach { e ->
                Column(Modifier.fillMaxWidth(), verticalArrangement = Arrangement.spacedBy(2.dp)) {
                    Text(
                        listOf(e.kind.replaceFirstChar { it.uppercase() }, e.model, onuStatusLabel(e.status)).filter { it.isNotBlank() }.joinToString("  \u00B7  "),
                        style = MaterialTheme.typography.bodyMedium,
                        color = MaterialTheme.colorScheme.onSurface,
                    )
                    if (e.serial.isNotBlank()) MonoText(e.serial, color = MaterialTheme.colorScheme.onSurfaceVariant, style = MaterialTheme.typography.bodySmall)
                    if (e.mac.isNotBlank()) MonoText(e.mac, color = MaterialTheme.colorScheme.onSurfaceVariant, style = MaterialTheme.typography.bodySmall)
                }
            }
        }
    }
}

@Composable
private fun DetailDialogs(ui: DetailUiState, vm: CustomerDetailViewModel, onOpenOther: (String) -> Unit) {
    val account = (ui.account as? Loadable.Ready)?.value ?: return
    when (val d = ui.dialog) {
        DetailDialog.None -> Unit
        DetailDialog.Extend -> PackageSheet(
            title = stringResource(if (account.expiresAt == null) R.string.action_activate else R.string.action_extend),
            confirm = stringResource(if (account.expiresAt == null) R.string.action_activate else R.string.action_extend),
            ui = ui,
            initial = account.pkg?.id,
            showNote = true,
            showKeepTime = false,
            onDismiss = vm::dismiss,
        ) { pkg, note, _ -> vm.extendTracked(pkg, note) }
        DetailDialog.ChangePackage -> PackageSheet(
            title = stringResource(R.string.change_plan),
            confirm = stringResource(R.string.action_change),
            ui = ui,
            initial = account.pkg?.id,
            showNote = false,
            showKeepTime = true,
            onDismiss = vm::dismiss,
        ) { pkg, _, keep -> vm.changePackage(pkg, keep) }
        DetailDialog.EditDetails -> DetailsSheet(account, ui, vm)
        DetailDialog.EditMac -> MacSheet(account, ui, vm, onOpenOther)
        DetailDialog.Move -> MoveSheet(account, ui, vm)
        is DetailDialog.ConfirmSuspend -> AlertDialog(
            onDismissRequest = vm::dismiss,
            title = {
                Text(
                    stringResource(
                        if (d.suspend) R.string.suspend_title else R.string.unsuspend_title,
                        account.unitLabel.ifBlank { account.code },
                    ),
                )
            },
            text = { Text(stringResource(if (d.suspend) R.string.suspend_body else R.string.unsuspend_body)) },
            confirmButton = {
                TextButton(
                    onClick = { vm.setSuspended(d.suspend) },
                    enabled = !ui.busy,
                    colors = if (d.suspend) ButtonDefaults.textButtonColors(contentColor = MaterialTheme.colorScheme.error) else ButtonDefaults.textButtonColors(),
                ) { Text(stringResource(if (d.suspend) R.string.action_suspend else R.string.action_unsuspend)) }
            },
            dismissButton = { TextButton(onClick = vm::dismiss) { Text(stringResource(R.string.action_cancel)) } },
        )
        is DetailDialog.LowerPlan -> AlertDialog(
            onDismissRequest = vm::dismiss,
            title = { Text(stringResource(R.string.better_plan_title)) },
            text = { Text(stringResource(R.string.better_plan_body, d.message)) },
            confirmButton = { TextButton(onClick = { vm.extend(d.packageId, d.note, force = true) }, enabled = !ui.busy) { Text(stringResource(R.string.apply_anyway)) } },
            dismissButton = { TextButton(onClick = vm::dismiss) { Text(stringResource(R.string.action_cancel)) } },
        )
    }
}

@Composable
private fun PackageSheet(
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
    FormSheet(
        title = title,
        onDismiss = onDismiss,
        confirmLabel = if (ui.busy) stringResource(R.string.working) else confirm,
        onConfirm = { selected?.let { onConfirm(it, note.ifBlank { null }, keep) } },
        confirmEnabled = selected != null && !ui.busy,
    ) {
        PackagePicker(ui.packages, selected, { selected = it })
        if (showKeepTime) CheckRow(keep, { keep = it }, stringResource(R.string.keep_remaining_time))
        if (showNote) {
            OutlinedTextField(note, { note = it }, label = { Text(stringResource(R.string.note_optional)) }, modifier = Modifier.fillMaxWidth())
        }
        ui.error?.let { Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodyMedium) }
    }
}

/** Edit tenant, phone, WhatsApp and notes, like the web's Details form. The house number is not here; Move changes it. */
@Composable
private fun DetailsSheet(account: Account, ui: DetailUiState, vm: CustomerDetailViewModel) {
    var tenant by remember { mutableStateOf(account.tenant) }
    var phone by remember { mutableStateOf(account.phone) }
    var same by remember { mutableStateOf(account.waSamePhone) }
    var whatsapp by remember { mutableStateOf(account.whatsapp) }
    var notes by remember { mutableStateOf(account.notes) }
    FormSheet(
        title = stringResource(R.string.edit_details),
        onDismiss = vm::dismiss,
        confirmLabel = stringResource(if (ui.busy) R.string.saving else R.string.action_save),
        onConfirm = { vm.saveDetails(tenant, phone, whatsapp, same, notes) },
        confirmEnabled = !ui.busy,
    ) {
        Text(
            stringResource(R.string.house_number_note, account.unitLabel.ifBlank { "-" }),
            style = MaterialTheme.typography.bodyMedium,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
        OutlinedTextField(tenant, { tenant = it }, label = { Text(stringResource(R.string.label_tenant)) }, modifier = Modifier.fillMaxWidth(), keyboardOptions = KeyboardOptions(capitalization = KeyboardCapitalization.Words))
        OutlinedTextField(
            phone, { phone = it },
            label = { Text(stringResource(R.string.label_phone)) },
            modifier = Modifier.fillMaxWidth(),
            textStyle = LocalTextStyle.current.copy(fontFamily = GeistMono),
            keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Phone),
        )
        SwitchRow(same, { same = it }, stringResource(R.string.whatsapp_same_as_phone))
        if (!same) {
            OutlinedTextField(
                whatsapp, { whatsapp = it },
                label = { Text(stringResource(R.string.label_whatsapp)) },
                modifier = Modifier.fillMaxWidth(),
                textStyle = LocalTextStyle.current.copy(fontFamily = GeistMono),
                keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Phone),
            )
        }
        OutlinedTextField(notes, { notes = it }, label = { Text(stringResource(R.string.label_notes)) }, minLines = 2, modifier = Modifier.fillMaxWidth())
        ui.error?.let { Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodyMedium) }
    }
}

@Composable
private fun MacSheet(account: Account, ui: DetailUiState, vm: CustomerDetailViewModel, onOpenOther: (String) -> Unit) {
    FormSheet(
        title = stringResource(R.string.edit_mac_for, account.unitLabel.ifBlank { account.code }),
        onDismiss = vm::dismiss,
        confirmLabel = stringResource(if (ui.busy) R.string.saving else R.string.action_save),
        onConfirm = vm::saveMac,
        confirmEnabled = !ui.busy && ui.macFieldError == null && ui.macInput.isNotBlank(),
    ) {
        Text(
            stringResource(R.string.mac_hint),
            style = MaterialTheme.typography.bodyMedium,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
        OutlinedTextField(
            value = ui.macInput,
            onValueChange = vm::onMacInput,
            label = { Text(stringResource(R.string.mac_address)) },
            placeholder = { Text("AA:BB:CC:DD:EE:FF") },
            singleLine = true,
            isError = ui.macFieldError != null || ui.macOwner != null,
            supportingText = { ui.macFieldError?.let { Text(it) } },
            textStyle = LocalTextStyle.current.copy(fontFamily = GeistMono),
            keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Ascii, capitalization = KeyboardCapitalization.Characters),
            modifier = Modifier.fillMaxWidth(),
        )
        ui.macOwner?.let { MacConflictNotice(it, onOpen = { id -> vm.dismiss(); onOpenOther(id) }) }
        ui.error?.let { Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodyMedium) }
    }
}

/** Shown for a 409 mac_in_use: says exactly which house and building already own the MAC. */
@Composable
fun MacConflictNotice(owner: MacOwner, onOpen: ((String) -> Unit)? = null) {
    Card(colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.errorContainer, contentColor = MaterialTheme.colorScheme.onErrorContainer)) {
        Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
            Text(stringResource(R.string.mac_in_use), style = MaterialTheme.typography.titleSmall)
            Text(macConflictText(owner), style = MaterialTheme.typography.bodyMedium)
            if (onOpen != null && owner.accountId.isNotBlank()) {
                TextButton(onClick = { onOpen(owner.accountId) }) { Text(stringResource(R.string.open_house, owner.flat.ifBlank { owner.code })) }
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

/** Move a customer to another apartment: pick it, say the house number, and the router when the apartment has several. */
@OptIn(ExperimentalLayoutApi::class)
@Composable
private fun MoveSheet(account: Account, ui: DetailUiState, vm: CustomerDetailViewModel) {
    var aptId by remember { mutableStateOf(account.apartment?.id.orEmpty()) }
    var unit by remember { mutableStateOf(account.unitLabel) }
    var routerId by remember { mutableStateOf<String?>(null) }
    var picking by remember { mutableStateOf(false) }
    val list = (ui.apartments as? Loadable.Ready)?.value.orEmpty()
    val chosen = list.firstOrNull { it.id == aptId }
    // One router: the server picks it. Several: the operator chooses.
    val needsRouter = (chosen?.routers?.size ?: 0) > 1
    FormSheet(
        title = stringResource(R.string.move_title, account.unitLabel.ifBlank { account.code }),
        onDismiss = vm::dismiss,
        confirmLabel = stringResource(if (ui.busy) R.string.moving else R.string.action_move),
        onConfirm = { vm.move(aptId, routerId, unit) },
        confirmEnabled = !ui.busy && chosen != null && chosen.routers.isNotEmpty() && unit.isNotBlank() && (!needsRouter || routerId != null),
    ) {
        Text(
            stringResource(R.string.move_note),
            style = MaterialTheme.typography.bodyMedium,
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
        OutlinedTextField(unit, { unit = it }, label = { Text(stringResource(R.string.house_number)) }, singleLine = true, modifier = Modifier.fillMaxWidth())
        if (needsRouter) {
            Text(stringResource(R.string.label_router), style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
            FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                chosen?.routers?.forEach { r ->
                    FilterChip(selected = routerId == r.key, onClick = { routerId = r.key }, label = { Text(r.label) })
                }
            }
        }
        if (chosen != null && chosen.routers.isEmpty()) {
            Text(stringResource(R.string.apartment_no_router), color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodyMedium)
        }
        ui.error?.let { Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodyMedium) }
    }
}
