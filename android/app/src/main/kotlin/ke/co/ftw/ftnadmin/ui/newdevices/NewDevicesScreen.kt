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
import androidx.compose.material.icons.filled.Share
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

@OptIn(ExperimentalMaterial3Api::class)
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
        topBar = { AppTopBar("New devices", onBack = onBack) },
        snackbarHost = { SnackbarHost(snack) },
    ) { inner ->
        when (val d = ui.devices) {
            Loadable.Loading -> CenteredLoading(Modifier.padding(inner))
            is Loadable.Failed -> ErrorState(d.message, vm::retry, Modifier.padding(inner))
            is Loadable.Ready -> PullToRefreshBox(ui.refreshing, vm::refresh, Modifier.padding(inner).fillMaxSize()) {
                if (d.value.isEmpty() && ui.registrations.isEmpty()) {
                    // Scrollable so pull-to-refresh still works on an empty list.
                    Column(Modifier.fillMaxSize().verticalScroll(rememberScrollState())) {
                        EmptyState("No unconfigured devices. Everything connected has an account.\nPull down to check the routers again.")
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
                                    "Waiting for approval",
                                    style = MaterialTheme.typography.titleMedium,
                                    color = MaterialTheme.colorScheme.onSurface,
                                )
                            }
                            if (user.canWrite) {
                                item(key = "regs-rejectall") {
                                    ChipRow(horizontalPadding = 0.dp) {
                                        rejectAllTargets(ui.registrations).forEach { t ->
                                            OutlinedButton(onClick = { vm.askRejectAll(t) }) { Text("Reject all ${t.count} for ${t.label}", maxLines = 1) }
                                        }
                                    }
                                }
                            }
                            items(ui.registrations, key = { "reg-" + it.id }) { r ->
                                RegistrationRow(r, canDecide = user.canWrite, onApprove = { vm.askApprove(r) }, onReject = { vm.askReject(r) })
                            }
                            item(key = "devices-title") {
                                Text(
                                    "Devices not on an account",
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
@Composable
private fun RegistrationRow(r: PendingRegistration, canDecide: Boolean, onApprove: () -> Unit, onReject: () -> Unit) {
    Card(modifier = Modifier.fillMaxWidth(), colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.secondaryContainer)) {
        Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(2.dp)) {
            Text(r.name, style = MaterialTheme.typography.titleMedium, color = MaterialTheme.colorScheme.onSecondaryContainer)
            Text(
                if (r.unassigned) listOf(r.building, "house ${r.unit}", "no router yet").filter { it.isNotBlank() }.joinToString("  |  ")
                else listOf(r.apartmentName, "house ${r.unit}", r.siteName).filter { it.isNotBlank() }.joinToString("  |  "),
                style = MaterialTheme.typography.bodyMedium,
                color = MaterialTheme.colorScheme.onSecondaryContainer,
            )
            Text(
                listOf(r.phone, r.email).filter { it.isNotBlank() }.joinToString("  |  "),
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSecondaryContainer,
            )
            if (r.deviceName.isNotBlank()) {
                Text("Device name given: ${r.deviceName}", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSecondaryContainer)
            }
            if (r.unassigned) {
                Text(
                    "Their address matches more than one router. Assign a router and apartment on the web admin (New devices) before approving.",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSecondaryContainer,
                )
            }
            if (canDecide) {
                Row(Modifier.padding(top = 8.dp), horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    Button(onClick = onApprove, enabled = !r.unassigned) { Text("Approve", maxLines = 1) }
                    OutlinedButton(onClick = onReject) { Text("Reject", maxLines = 1) }
                }
            }
        }
    }
}

/**
 * The server cannot see the visitor's MAC, so the owner names the device: one of the router's unconfigured devices
 * (the likely one is preselected) or a MAC typed in.
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun ApproveDialog(r: PendingRegistration, mac: String, busy: Boolean, vm: NewDevicesViewModel) {
    AlertDialog(
        onDismissRequest = vm::cancelApprove,
        title = { Text("Approve ${r.name}?") },
        text = {
            Column(Modifier.verticalScroll(rememberScrollState()), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                Text("House ${r.unit} gets an account on the device you pick. It comes online on the next router check.", style = MaterialTheme.typography.bodyMedium)
                if (r.deviceName.isNotBlank()) {
                    Text("Device name given: ${r.deviceName}", style = MaterialTheme.typography.bodyMedium, fontWeight = FontWeight.Bold)
                }
                if (r.candidates.isEmpty()) {
                    Text("No unknown device has been seen on this router yet. Type the MAC below.", style = MaterialTheme.typography.bodySmall)
                } else {
                    Text(
                        "Devices on this router, first seen closest to the request first. One is ticked only when it is the only device first seen within 10 minutes of the request.",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                    Column(Modifier.selectableGroup()) {
                        r.candidates.forEach { c ->
                            val picked = mac.equals(c.mac, ignoreCase = true)
                            Row(
                                Modifier.fillMaxWidth().selectable(selected = picked, onClick = { vm.setApproveMac(c.mac) }, role = Role.RadioButton).padding(vertical = 4.dp),
                                verticalAlignment = Alignment.Top,
                            ) {
                                RadioButton(selected = picked, onClick = null, modifier = Modifier.padding(top = 2.dp, end = 8.dp))
                                Column {
                                    Text(
                                        listOf(c.hostName.ifBlank { "no host name" }, c.vendor.ifBlank { "unknown vendor" }, c.port.takeIf { it.isNotBlank() }?.let { "port $it" }).filterNotNull().joinToString("  |  "),
                                        style = MaterialTheme.typography.bodyMedium,
                                        fontWeight = if (c.nameMatch) FontWeight.Bold else FontWeight.Normal,
                                        color = if (c.nameMatch) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.onSurface,
                                    )
                                    Text(
                                        listOf(c.mac, "first seen ${formatAge(c.firstSeen)}", "last seen ${formatAge(c.lastSeen)}", if (c.online) "online" else null)
                                            .filterNotNull().joinToString("  |  "),
                                        style = MaterialTheme.typography.bodySmall,
                                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                                    )
                                    val notes = listOfNotNull(if (c.nameMatch) "matches the name given" else null, if (c.likely) "likely" else null)
                                    if (notes.isNotEmpty()) {
                                        Text(notes.joinToString(", "), style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.primary)
                                    }
                                }
                            }
                        }
                    }
                }
                OutlinedTextField(mac, vm::setApproveMac, label = { Text("Device MAC") }, singleLine = true, modifier = Modifier.fillMaxWidth())
            }
        },
        confirmButton = { TextButton(onClick = vm::approve, enabled = mac.isNotBlank() && !busy) { Text(if (busy) "Approving..." else "Approve") } },
        dismissButton = { TextButton(onClick = vm::cancelApprove) { Text("Cancel") } },
    )
}

@Composable
private fun DeviceRow(d: NewDevice, onClick: () -> Unit) {
    val clipboard = LocalClipboardManager.current
    val good = MaterialTheme.colorScheme.tertiary
    val off = MaterialTheme.colorScheme.outline
    Card(onClick = onClick, modifier = Modifier.fillMaxWidth(), colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceContainer)) {
        Row(Modifier.padding(12.dp), verticalAlignment = Alignment.CenterVertically) {
            Box(Modifier.size(10.dp).background(if (d.online) good else off, CircleShape))
            Column(Modifier.weight(1f).padding(start = 12.dp), verticalArrangement = Arrangement.spacedBy(2.dp)) {
                Text(d.mac, style = MaterialTheme.typography.titleMedium, color = MaterialTheme.colorScheme.onSurface)
                Text(
                    listOf(d.vendor.ifBlank { "Unknown vendor" }, d.hostName, d.ip.ifBlank { d.addressNote }).filter { it.isNotBlank() }.joinToString("  |  "),
                    style = MaterialTheme.typography.bodyMedium,
                    color = MaterialTheme.colorScheme.onSurface,
                )
                Text(
                    listOf(d.routerLabel, if (d.online) "online" else "seen ${formatAge(d.lastSeen)}", d.suggestedAccount?.let { "looks like house ${it.flat}" }).filterNotNull().filter { it.isNotBlank() }.joinToString("  |  "),
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
                        fontWeight = FontWeight.Bold,
                        color = MaterialTheme.colorScheme.error,
                    )
                }
            }
            IconButton(onClick = { clipboard.setText(AnnotatedString(d.mac)) }) { Icon(Icons.Default.Share, contentDescription = "Copy MAC") }
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
            Modifier.fillMaxWidth().verticalScroll(rememberScrollState()).padding(horizontal = 16.dp, vertical = 8.dp),
            verticalArrangement = Arrangement.spacedBy(10.dp),
        ) {
            Text(d.mac, style = MaterialTheme.typography.titleLarge, color = MaterialTheme.colorScheme.onSurface)
            Text(
                listOf(d.vendor, d.hostName, d.ip.ifBlank { d.addressNote }, d.routerLabel).filter { it.isNotBlank() }.joinToString("  |  "),
                style = MaterialTheme.typography.bodyMedium,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            val belongs = d.belongsTo
            if (d.deviceClass.label.isNotBlank()) {
                Text(d.deviceClass.label, style = MaterialTheme.typography.labelLarge, color = MaterialTheme.colorScheme.primary)
            }
            if (d.deviceClass.isOlt) {
                Text(
                    "This is an OLT of the equipment register, not a customer. It can only be dismissed from this list.",
                    style = MaterialTheme.typography.bodyMedium,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            } else if (belongs != null) {
                BelongsToSection(sheet, belongs, vm)
            } else {
            ChipRow(horizontalPadding = 0.dp) {
                FilterChip(sheet.tab == SheetTab.Attach, { vm.setTab(SheetTab.Attach) }, label = { ChipLabel("Attach to account") })
                FilterChip(sheet.tab == SheetTab.Create, { vm.setTab(SheetTab.Create) }, label = { ChipLabel("Create account") })
            }
            when (sheet.tab) {
                SheetTab.Attach -> {
                    AccountPicker(
                        selectedId = sheet.selected?.id,
                        onSelect = vm::select,
                        search = vm::searchAccounts,
                        suggested = d.suggestedAccount?.let { ke.co.ftw.ftnadmin.domain.Account(id = it.id, code = it.code, flat = it.flat, siteId = d.routerKey) },
                    )
                    Button(onClick = vm::attach, enabled = sheet.selected != null && !sheet.busy, modifier = Modifier.fillMaxWidth()) {
                        Text(if (sheet.busy) "Attaching..." else "Attach to house ${sheet.selected?.flat.orEmpty()}".trim())
                    }
                }
                SheetTab.Create -> {
                    Text("Apartment", style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
                    if (sheet.apartments.isEmpty()) {
                        Text(
                            "This router serves no apartment yet. Link it to one on the Apartments page first.",
                            style = MaterialTheme.typography.bodySmall,
                            color = MaterialTheme.colorScheme.error,
                        )
                    }
                    ChipRow(horizontalPadding = 0.dp) {
                        sheet.apartments.forEach { a ->
                            FilterChip(sheet.apartmentId == a.id, { vm.setApartment(a.id) }, label = { ChipLabel(a.name) })
                        }
                    }
                    OutlinedTextField(sheet.flat, vm::setFlat, label = { Text("House number") }, singleLine = true, modifier = Modifier.fillMaxWidth())
                    OutlinedTextField(sheet.tenant, vm::setTenant, label = { Text("Tenant (optional)") }, singleLine = true, modifier = Modifier.fillMaxWidth())
                    OutlinedTextField(
                        sheet.phone, vm::setPhone, label = { Text("Phone (optional)") }, singleLine = true,
                        keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Phone), modifier = Modifier.fillMaxWidth(),
                    )
                    Text("Package (optional)", style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
                    PackagePicker(packages, sheet.packageId, vm::setPackage)
                    Button(onClick = vm::create, enabled = sheet.flat.isNotBlank() && sheet.apartmentId != null && !sheet.busy, modifier = Modifier.fillMaxWidth()) {
                        Text(if (sheet.busy) "Creating..." else "Create account")
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
            OutlinedButton(onClick = vm::askDismiss, enabled = !sheet.busy, modifier = Modifier.fillMaxWidth()) { Text("Dismiss this device") }
            Box(Modifier.size(16.dp))
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
@Composable
private fun BelongsToSection(sheet: DeviceSheet, b: ke.co.ftw.ftnadmin.domain.BelongsTo, vm: NewDevicesViewModel) {
    Card(colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.errorContainer, contentColor = MaterialTheme.colorScheme.onErrorContainer)) {
        Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
            Text(b.text, style = MaterialTheme.typography.titleSmall)
            Text(
                "It stays unregistered on this router. A MAC of one apartment is never set up in another.",
                style = MaterialTheme.typography.bodyMedium,
            )
        }
    }
    Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
        OutlinedButton(onClick = {}, enabled = false) { Text("Attach", maxLines = 1) }
        OutlinedButton(onClick = {}, enabled = false) { Text("Create account", maxLines = 1) }
    }
    if (!b.sameApartment && b.moveToChoices.size > 1) {
        Text("Move into apartment", style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
        ChipRow(horizontalPadding = 0.dp) {
            b.moveToChoices.forEach { c ->
                FilterChip(sheet.moveApartmentId == c.id, { vm.setMoveApartment(c.id) }, label = { ChipLabel(c.name) })
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
