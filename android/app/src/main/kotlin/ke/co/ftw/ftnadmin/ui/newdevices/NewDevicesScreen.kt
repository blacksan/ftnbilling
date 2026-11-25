package ke.co.ftw.ftnadmin.ui.newdevices

import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.selection.selectable
import androidx.compose.foundation.selection.selectableGroup
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FilterChip
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.ModalBottomSheet
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.RadioButton
import androidx.compose.material3.Scaffold
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.pulltorefresh.PullToRefreshBox
import androidx.compose.material3.rememberModalBottomSheetState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalClipboardManager
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.text.AnnotatedString
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.ui.unit.dp
import androidx.compose.ui.text.font.FontWeight
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import ke.co.ftw.ftnadmin.core.network.UserInfo
import ke.co.ftw.ftnadmin.domain.NewDevice
import ke.co.ftw.ftnadmin.domain.PendingRegistration
import ke.co.ftw.ftnadmin.ui.common.AccountPicker
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.imePadding
import androidx.compose.material.icons.filled.ContentCopy
import androidx.compose.material3.LocalTextStyle
import androidx.compose.material3.SegmentedButton
import androidx.compose.material3.SegmentedButtonDefaults
import androidx.compose.material3.SingleChoiceSegmentedButtonRow
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.setValue
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.input.KeyboardCapitalization
import androidx.compose.ui.text.style.TextOverflow
import ke.co.ftw.ftnadmin.R
import ke.co.ftw.ftnadmin.ui.common.ApartmentField
import ke.co.ftw.ftnadmin.ui.common.ApartmentPickerSheet
import ke.co.ftw.ftnadmin.ui.common.FormSheet
import ke.co.ftw.ftnadmin.ui.common.MonoText
import ke.co.ftw.ftnadmin.ui.common.RadioRow
import ke.co.ftw.ftnadmin.ui.common.StatusDot
import ke.co.ftw.ftnadmin.ui.common.houseText
import ke.co.ftw.ftnadmin.ui.theme.GeistMono
import ke.co.ftw.ftnadmin.ui.common.AppTopBar
import ke.co.ftw.ftnadmin.ui.common.CenteredLoading
import ke.co.ftw.ftnadmin.ui.common.ChipLabel
import ke.co.ftw.ftnadmin.ui.common.ChipRow
import ke.co.ftw.ftnadmin.ui.common.EmptyState
import ke.co.ftw.ftnadmin.ui.common.ErrorState
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.common.LocalContainer
import ke.co.ftw.ftnadmin.ui.common.PackagePicker
import ke.co.ftw.ftnadmin.ui.common.formatAge
import ke.co.ftw.ftnadmin.ui.common.vmFactory
import ke.co.ftw.ftnadmin.ui.customers.MacConflictNotice

@OptIn(ExperimentalMaterial3Api::class, ExperimentalLayoutApi::class)
@Composable
fun NewDevicesScreen(siteId: String?, user: UserInfo, onBack: () -> Unit, onOpenAccount: (String) -> Unit) {
    val container = LocalContainer.current
    val vm: NewDevicesViewModel = viewModel(
        key = "newdevices-${siteId.orEmpty()}",
        factory = vmFactory { NewDevicesViewModel(siteId, container.newDevices, container.sites, container.accounts, container.apartments) },
    )
    val ui by vm.state.collectAsStateWithLifecycle()
    val snack = remember { SnackbarHostState() }
    LaunchedEffect(ui.message) {
        ui.message?.let {
            snack.showSnackbar(it)
            vm.clearMessage()
        }
    }

    Scaffold(
        containerColor = MaterialTheme.colorScheme.background,
        topBar = { AppTopBar(stringResource(R.string.new_devices), onBack = onBack) },
        snackbarHost = { SnackbarHost(snack) },
    ) { inner ->
        when (val d = ui.devices) {
            Loadable.Loading -> CenteredLoading(Modifier.padding(inner))
            is Loadable.Failed -> ErrorState(d.message, vm::retry, Modifier.padding(inner))
            is Loadable.Ready -> PullToRefreshBox(ui.refreshing, vm::refresh, Modifier.padding(inner).fillMaxSize()) {
                if (d.value.isEmpty() && ui.registrations.isEmpty()) {
                    // Scrollable so pull-to-refresh still works on an empty list.
                    Column(Modifier.fillMaxSize().verticalScroll(rememberScrollState())) {
                        EmptyState(stringResource(R.string.no_new_devices), body = stringResource(R.string.pull_to_recheck))
                    }
                } else {
                    LazyColumn(
                        Modifier.fillMaxSize(),
                        contentPadding = androidx.compose.foundation.layout.PaddingValues(16.dp),
                        verticalArrangement = Arrangement.spacedBy(8.dp),
                    ) {
                        if (ui.registrations.isNotEmpty()) {
                            item(key = "regs-title") {
                                Text(
                                    stringResource(R.string.waiting_for_approval),
                                    style = MaterialTheme.typography.titleMedium,
                                    color = MaterialTheme.colorScheme.onSurface,
                                )
                            }
                            if (user.canWrite) {
                                item(key = "regs-rejectall") {
                                    FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                                        rejectAllTargets(ui.registrations).forEach { t ->
                                            OutlinedButton(onClick = { vm.askRejectAll(t) }) { Text(stringResource(R.string.reject_all_for, t.count, t.label)) }
                                        }
                                    }
                                }
                            }
                            items(ui.registrations, key = { "reg-" + it.id }) { r ->
                                RegistrationRow(r, canDecide = user.canWrite, onApprove = { vm.askApprove(r) }, onReject = { vm.askReject(r) })
                            }
                            item(key = "devices-title") {
                                Text(
                                    stringResource(R.string.devices_not_on_account),
                                    style = MaterialTheme.typography.titleMedium,
                                    color = MaterialTheme.colorScheme.onSurface,
                                )
                            }
                        }
                        items(d.value, key = { it.routerKey + it.mac }) { dev -> DeviceRow(dev, onClick = { if (user.canWrite) vm.open(dev) }) }
                    }
                }
            }
        }
    }

    ui.sheet?.let { sheet -> DeviceSheetContent(sheet, ui.packages, vm, onOpenAccount) }
    ui.approving?.let { r -> ApproveDialog(r, ui.approveMac, ui.decideBusy, vm) }
    ui.rejectingAll?.let { t ->
        AlertDialog(
            onDismissRequest = vm::cancelRejectAll,
            title = { Text("Reject all ${t.count} for ${t.label}?") },
            text = { Text("Every waiting registration there is declined and the people's details are erased. Their devices stay offline. They can register again.") },
            confirmButton = { TextButton(onClick = vm::rejectAll, enabled = !ui.decideBusy) { Text("Reject all") } },
            dismissButton = { TextButton(onClick = vm::cancelRejectAll) { Text("Cancel") } },
        )
    }
    ui.rejecting?.let { r ->
        AlertDialog(
            onDismissRequest = vm::cancelReject,
            title = { Text("Reject ${r.name}?") },
            text = { Text("Their details are erased. The device stays offline. They can register again.") },
            confirmButton = { TextButton(onClick = vm::reject, enabled = !ui.decideBusy) { Text("Reject") } },
            dismissButton = { TextButton(onClick = vm::cancelReject) { Text("Cancel") } },
        )
    }
}

/** One person who registered on the captive page. Approve asks which device is theirs. */
@OptIn(ExperimentalLayoutApi::class)
@Composable
private fun RegistrationRow(r: PendingRegistration, canDecide: Boolean, onApprove: () -> Unit, onReject: () -> Unit) {
    Card(modifier = Modifier.fillMaxWidth(), colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.secondaryContainer)) {
        Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(2.dp)) {
            Text(r.name, style = MaterialTheme.typography.titleMedium, color = MaterialTheme.colorScheme.onSecondaryContainer)
            Text(
                if (r.unassigned) listOf(r.building, houseText(r.unit), "no router yet").filter { it.isNotBlank() }.joinToString("  |  ")
                else listOf(r.apartmentName, houseText(r.unit), r.siteName).filter { it.isNotBlank() }.joinToString("  |  "),
                style = MaterialTheme.typography.bodyMedium,
                color = MaterialTheme.colorScheme.onSecondaryContainer,
            )
            Text(
                listOf(r.phone, r.email).filter { it.isNotBlank() }.joinToString("  |  "),
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSecondaryContainer,
            )
            if (r.deviceName.isNotBlank()) {
                Text(stringResource(R.string.device_name_given, r.deviceName), style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSecondaryContainer)
            }
            if (r.unassigned) {
                Text(
                    "Their address matches more than one router. Assign a router and apartment on the web admin (New devices) before approving.",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSecondaryContainer,
                )
            }
            if (canDecide) {
                FlowRow(Modifier.padding(top = 8.dp), horizontalArrangement = Arrangement.spacedBy(8.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                    Button(onClick = onApprove, enabled = !r.unassigned) { Text(stringResource(R.string.approve)) }
                    OutlinedButton(onClick = onReject) { Text(stringResource(R.string.reject)) }
                }
            }
        }
    }
}

/**
 * The server cannot see the visitor's MAC, so the owner names the device: one of the router's unconfigured devices
 * (the likely one is preselected) or a MAC typed in.
 */
@Composable
private fun ApproveDialog(r: PendingRegistration, mac: String, busy: Boolean, vm: NewDevicesViewModel) {
    // A scrolling sheet: the candidate list and the MAC field stay reachable at large fonts and above the keyboard.
    FormSheet(
        title = stringResource(R.string.approve_title, r.name),
        onDismiss = vm::cancelApprove,
        confirmLabel = stringResource(if (busy) R.string.approving else R.string.approve),
        onConfirm = vm::approve,
        confirmEnabled = mac.isNotBlank() && !busy,
    ) {
        Text(stringResource(R.string.approve_body, houseText(r.unit)), style = MaterialTheme.typography.bodyMedium)
        if (r.deviceName.isNotBlank()) {
            Text(stringResource(R.string.device_name_given, r.deviceName), style = MaterialTheme.typography.bodyMedium, fontWeight = FontWeight.SemiBold)
        }
        if (r.candidates.isEmpty()) {
            Text(stringResource(R.string.no_unknown_device), style = MaterialTheme.typography.bodyMedium)
        } else {
            Text(
                stringResource(R.string.candidates_hint),
                style = MaterialTheme.typography.bodyMedium,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            Column(Modifier.selectableGroup()) {
                r.candidates.forEach { c ->
                    val picked = mac.equals(c.mac, ignoreCase = true)
                    RadioRow(selected = picked, onSelect = { vm.setApproveMac(c.mac) }) {
                        Column(Modifier.weight(1f)) {
                            Text(
                                listOf(c.hostName.ifBlank { "no host name" }, c.vendor.ifBlank { "unknown vendor" }, c.port.takeIf { it.isNotBlank() }?.let { "port $it" }).filterNotNull().joinToString("  |  "),
                                style = MaterialTheme.typography.bodyMedium,
                                fontWeight = if (c.nameMatch) FontWeight.SemiBold else FontWeight.Normal,
                                color = if (c.nameMatch) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.onSurface,
                            )
                            MonoText(c.mac, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                            Text(
                                listOf("first seen ${formatAge(c.firstSeen)}", "last seen ${formatAge(c.lastSeen)}", if (c.online) "online" else null).filterNotNull().joinToString("  |  "),
                                style = MaterialTheme.typography.bodySmall,
                                color = MaterialTheme.colorScheme.onSurfaceVariant,
                            )
                            val notes = listOfNotNull(if (c.nameMatch) "matches the name given" else null, if (c.likely) "likely" else null)
                            if (notes.isNotEmpty()) {
                                Text(notes.joinToString(", "), style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.primary)
                            }
                        }
                    }
                }
            }
        }
        OutlinedTextField(
            mac, vm::setApproveMac, label = { Text(stringResource(R.string.device_mac)) }, singleLine = true,
            textStyle = LocalTextStyle.current.copy(fontFamily = GeistMono),
            keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Ascii, capitalization = KeyboardCapitalization.Characters),
            modifier = Modifier.fillMaxWidth(),
        )
    }
}

@Composable
private fun DeviceRow(d: NewDevice, onClick: () -> Unit) {
    val clipboard = LocalClipboardManager.current
    Card(onClick = onClick, modifier = Modifier.fillMaxWidth(), colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceContainer)) {
        Row(Modifier.padding(12.dp), verticalAlignment = Alignment.CenterVertically) {
            // The same dot as everywhere else (green online); the line below says "online" or when it was last seen.
            StatusDot(if (d.online) true else null, describe = false)
            Column(Modifier.weight(1f).padding(start = 12.dp), verticalArrangement = Arrangement.spacedBy(2.dp)) {
                MonoText(d.mac, style = MaterialTheme.typography.titleMedium, color = MaterialTheme.colorScheme.onSurface)
                Text(
                    listOf(d.vendor.ifBlank { "Unknown vendor" }, d.hostName, d.ip.ifBlank { d.addressNote }).filter { it.isNotBlank() }.joinToString("  |  "),
                    style = MaterialTheme.typography.bodyMedium,
                    color = MaterialTheme.colorScheme.onSurface,
                )
                Text(
                    listOf(d.routerLabel, if (d.online) "online" else "seen ${formatAge(d.lastSeen)}", d.suggestedAccount?.let { "looks like ${houseText(it.flat).lowercase()}" }).filterNotNull().filter { it.isNotBlank() }.joinToString("  |  "),
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
                if (d.deviceClass.label.isNotBlank() && !d.deviceClass.kind.equals("unknown", true)) {
                    Text(d.deviceClass.label, style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.primary)
                }
                d.belongsTo?.let { b ->
                    Text(
                        b.text,
                        style = MaterialTheme.typography.bodySmall,
                        fontWeight = FontWeight.SemiBold,
                        color = MaterialTheme.colorScheme.error,
                    )
                }
            }
            IconButton(onClick = { clipboard.setText(AnnotatedString(d.mac)) }) { Icon(Icons.Default.ContentCopy, contentDescription = stringResource(R.string.copy_mac)) }
        }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun DeviceSheetContent(
    sheet: DeviceSheet,
    packages: List<ke.co.ftw.ftnadmin.domain.ServicePackage>,
    vm: NewDevicesViewModel,
    onOpenAccount: (String) -> Unit,
) {
    val d = sheet.device
    ModalBottomSheet(onDismissRequest = vm::closeSheet, sheetState = rememberModalBottomSheetState(skipPartiallyExpanded = true)) {
        Column(
            Modifier.fillMaxWidth().imePadding().verticalScroll(rememberScrollState()).padding(horizontal = 16.dp).padding(top = 8.dp, bottom = 16.dp),
            verticalArrangement = Arrangement.spacedBy(10.dp),
        ) {
            MonoText(d.mac, style = MaterialTheme.typography.titleLarge, color = MaterialTheme.colorScheme.onSurface)
            Text(
                listOf(d.vendor, d.hostName, d.ip.ifBlank { d.addressNote }, d.routerLabel).filter { it.isNotBlank() }.joinToString("  |  "),
                style = MaterialTheme.typography.bodyMedium,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            val belongs = d.belongsTo
            if (d.deviceClass.label.isNotBlank()) {
                Text(d.deviceClass.label, style = MaterialTheme.typography.labelLarge, color = MaterialTheme.colorScheme.primary)
            }
            if (d.deviceClass.isOlt || d.deviceClass.isOnuMgmt) {
                Text(
                    if (d.deviceClass.isOnuMgmt) {
                        "This is an ONU's TR-069 management address, not a customer. It can only be dismissed from this list."
                    } else {
                        "This is an OLT of the equipment register, not a customer. It can only be dismissed from this list."
                    },
                    style = MaterialTheme.typography.bodyMedium,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            } else if (belongs != null) {
                BelongsToSection(sheet, belongs, vm)
            } else {
            SingleChoiceSegmentedButtonRow(Modifier.fillMaxWidth()) {
                SegmentedButton(
                    selected = sheet.tab == SheetTab.Attach,
                    onClick = { vm.setTab(SheetTab.Attach) },
                    shape = SegmentedButtonDefaults.itemShape(0, 2),
                ) { Text(stringResource(R.string.attach_to_account), maxLines = 2, overflow = TextOverflow.Ellipsis) }
                SegmentedButton(
                    selected = sheet.tab == SheetTab.Create,
                    onClick = { vm.setTab(SheetTab.Create) },
                    shape = SegmentedButtonDefaults.itemShape(1, 2),
                ) { Text(stringResource(R.string.create_account), maxLines = 2, overflow = TextOverflow.Ellipsis) }
            }
            when (sheet.tab) {
                SheetTab.Attach -> {
                    AccountPicker(
                        selectedId = sheet.selected?.id,
                        onSelect = vm::select,
                        search = vm::searchAccounts,
                        suggested = d.suggestedAccount?.let { ke.co.ftw.ftnadmin.domain.Account(id = it.id, code = it.code, flat = it.flat, unit = it.flat, siteId = d.routerKey) },
                    )
                    Button(onClick = vm::attach, enabled = sheet.selected != null && !sheet.busy, modifier = Modifier.fillMaxWidth()) {
                        Text(if (sheet.busy) stringResource(R.string.attaching) else stringResource(R.string.attach_to_house, sheet.selected?.unitLabel.orEmpty()).trim())
                    }
                }
                SheetTab.Create -> {
                    if (sheet.apartments.isEmpty()) {
                        Text(
                            stringResource(R.string.router_no_apartment),
                            style = MaterialTheme.typography.bodyMedium,
                            color = MaterialTheme.colorScheme.error,
                        )
                    } else {
                        // The same apartment picker as Add customer and Move.
                        var pickingApartment by remember { mutableStateOf(false) }
                        ApartmentField(selected = sheet.apartments.firstOrNull { it.id == sheet.apartmentId }, onClick = { pickingApartment = true })
                        if (pickingApartment) {
                            ApartmentPickerSheet(
                                apartments = sheet.apartments,
                                selectedId = sheet.apartmentId,
                                onSelect = { a -> vm.setApartment(a.id); pickingApartment = false },
                                onDismiss = { pickingApartment = false },
                            )
                        }
                    }
                    OutlinedTextField(sheet.flat, vm::setFlat, label = { Text(stringResource(R.string.house_number)) }, singleLine = true, modifier = Modifier.fillMaxWidth())
                    OutlinedTextField(sheet.tenant, vm::setTenant, label = { Text(stringResource(R.string.tenant_optional)) }, singleLine = true, modifier = Modifier.fillMaxWidth())
                    OutlinedTextField(
                        sheet.phone, vm::setPhone, label = { Text(stringResource(R.string.phone_optional)) }, singleLine = true,
                        keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Phone), modifier = Modifier.fillMaxWidth(),
                    )
                    Text(stringResource(R.string.plan_optional), style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
                    PackagePicker(packages, sheet.packageId, vm::setPackage)
                    Button(onClick = vm::create, enabled = sheet.flat.isNotBlank() && sheet.apartmentId != null && !sheet.busy, modifier = Modifier.fillMaxWidth()) {
                        Text(stringResource(if (sheet.busy) R.string.creating else R.string.create_account))
                    }
                }
            }
            }
            sheet.macOther?.let { o ->
                Text(
                    "${o.text}. This MAC cannot be used in another apartment. Use Move customer here on the device instead.",
                    color = MaterialTheme.colorScheme.error,
                    style = MaterialTheme.typography.bodyMedium,
                )
            }
            sheet.macOwner?.let { MacConflictNotice(it, onOpen = { id -> vm.closeSheet(); onOpenAccount(id) }) }
            sheet.error?.let { Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodyMedium) }
            OutlinedButton(onClick = vm::askDismiss, enabled = !sheet.busy, modifier = Modifier.fillMaxWidth()) { Text(stringResource(R.string.dismiss_device)) }
        }
    }
    if (sheet.confirmMove) {
        val b = d.belongsTo
        AlertDialog(
            onDismissRequest = vm::cancelMove,
            title = { Text("Move ${b?.customer?.ifBlank { "this customer" }.orEmpty()} here?") },
            text = {
                Text(
                    "Their lease and queue are removed from ${b?.siteName.orEmpty()} and written on ${d.routerLabel}. " +
                        "Their apartment becomes the one you picked. This is kept in their history.",
                )
            },
            confirmButton = { TextButton(onClick = vm::moveHere) { Text("Move") } },
            dismissButton = { TextButton(onClick = vm::cancelMove) { Text("Cancel") } },
        )
    }
    if (sheet.confirmNoAddress) {
        AlertDialog(
            onDismissRequest = vm::cancelConfirmNoAddress,
            title = { Text("No address yet") },
            text = {
                Text(
                    "${d.mac} was only seen at layer 2 and has not asked the router for an address. " +
                        "It may be a bridge-only ONU or an OLT. Create the customer anyway?",
                )
            },
            confirmButton = { TextButton(onClick = vm::confirmCreateWithoutAddress) { Text("Create anyway") } },
            dismissButton = { TextButton(onClick = vm::cancelConfirmNoAddress) { Text("Cancel") } },
        )
    }
    if (sheet.confirmDismiss) {
        AlertDialog(
            onDismissRequest = vm::cancelDismiss,
            title = { Text("Dismiss ${d.mac}?") },
            text = { Text("It is hidden from this list, for example a neighbour's router or a test device. It is not given any access.") },
            confirmButton = { TextButton(onClick = vm::dismissDevice) { Text("Dismiss") } },
            dismissButton = { TextButton(onClick = vm::cancelDismiss) { Text("Cancel") } },
        )
    }
}

/**
 * The MAC belongs to a customer of another apartment, so it is never provisioned here. Attach and Create are
 * disabled; the owner or an operator can move the customer to this router after a confirm.
 */
@OptIn(ExperimentalLayoutApi::class)
@Composable
private fun BelongsToSection(sheet: DeviceSheet, b: ke.co.ftw.ftnadmin.domain.BelongsTo, vm: NewDevicesViewModel) {
    Card(colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.errorContainer, contentColor = MaterialTheme.colorScheme.onErrorContainer)) {
        Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
            Text(b.text, style = MaterialTheme.typography.titleSmall)
            Text(
                "It stays unregistered on this router. A MAC of one apartment is never set up in another. Attach and Create are not offered.",
                style = MaterialTheme.typography.bodyMedium,
            )
        }
    }
    if (!b.sameApartment && b.moveToChoices.size > 1) {
        Text("Move into apartment", style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
        FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            b.moveToChoices.forEach { c ->
                FilterChip(sheet.moveApartmentId == c.id, { vm.setMoveApartment(c.id) }, label = { Text(c.name) })
            }
        }
    }
    val needsChoice = !b.sameApartment && b.moveToChoices.size > 1 && sheet.moveApartmentId == null
    Button(onClick = vm::askMove, enabled = !sheet.busy && !needsChoice && (b.sameApartment || b.moveToChoices.isNotEmpty()), modifier = Modifier.fillMaxWidth()) {
        Text(if (sheet.busy) "Moving..." else "Move customer here")
    }
    if (!b.sameApartment && b.moveToChoices.isEmpty()) {
        Text(
            "This router serves no apartment. Link it to the customer's apartment, or to another one, on the Apartments page first.",
            style = MaterialTheme.typography.bodySmall,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
    }
}
