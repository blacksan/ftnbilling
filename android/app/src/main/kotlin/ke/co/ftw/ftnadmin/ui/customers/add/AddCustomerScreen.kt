package ke.co.ftw.ftnadmin.ui.customers.add

import androidx.compose.foundation.BorderStroke
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Check
import androidx.compose.material.icons.filled.Refresh
import androidx.compose.material.icons.filled.Search
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.FilterChip
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Surface
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalClipboardManager
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.KeyboardCapitalization
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import ke.co.ftw.ftnadmin.data.routers.Client
import ke.co.ftw.ftnadmin.domain.ServicePackage
import ke.co.ftw.ftnadmin.ui.common.AppTopBar
import ke.co.ftw.ftnadmin.ui.common.ChipLabel
import ke.co.ftw.ftnadmin.ui.common.ChipRow
import ke.co.ftw.ftnadmin.ui.common.LabeledValue
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.common.LoadingSpinner
import ke.co.ftw.ftnadmin.ui.common.LocalContainer
import ke.co.ftw.ftnadmin.ui.common.StatusChip
import ke.co.ftw.ftnadmin.ui.common.StatusDot
import ke.co.ftw.ftnadmin.ui.common.Tone
import ke.co.ftw.ftnadmin.ui.common.formatDuration
import ke.co.ftw.ftnadmin.ui.common.formatKes
import ke.co.ftw.ftnadmin.ui.common.vmFactory
import ke.co.ftw.ftnadmin.ui.customers.macConflictText

@Composable
fun AddCustomerScreen(routerId: String?, mac: String?, onBack: () -> Unit, onOpenAccount: (String) -> Unit) {
    val container = LocalContainer.current
    val vm: AddCustomerViewModel = viewModel(
        key = "add-customer-${routerId.orEmpty()}-${mac.orEmpty()}",
        factory = vmFactory {
            AddCustomerViewModel(
                container.accounts, container.routerLive, container.sites, container.apartments,
                initialRouterId = routerId ?: container.routerContext.selection.value,
                initialMac = mac,
            )
        },
    )
    val ui by vm.state.collectAsStateWithLifecycle()

    Scaffold(
        containerColor = MaterialTheme.colorScheme.background,
        topBar = { AppTopBar("Add customer", onBack = onBack) },
        bottomBar = {
            if (ui.added == null) {
                Surface(color = MaterialTheme.colorScheme.surfaceContainer, tonalElevation = 3.dp) {
                    Button(
                        onClick = vm::submit,
                        enabled = ui.canSubmit,
                        modifier = Modifier.fillMaxWidth().navigationBarsPadding().padding(16.dp),
                    ) {
                        Text(if (ui.submitting) "Creating..." else if (ui.activate) "Create and activate" else "Create customer", fontWeight = FontWeight.ExtraBold, maxLines = 1)
                    }
                }
            }
        },
    ) { inner ->
        Column(
            Modifier.padding(inner).fillMaxSize().imePadding().verticalScroll(rememberScrollState()).padding(16.dp),
            verticalArrangement = Arrangement.spacedBy(16.dp),
        ) {
            val added = ui.added
            if (added != null) {
                ResultCard(added, onOpen = { onOpenAccount(added.account.id) }, onAnother = vm::addAnother, onRecheck = vm::recheck, onDone = onBack)
            } else {
                Form(ui, vm)
            }
        }
    }
}

@Composable
private fun StepCard(number: Int, title: String, content: @Composable () -> Unit) {
    Card(
        modifier = Modifier.fillMaxWidth(),
        colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceContainerHigh),
    ) {
        Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(10.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(10.dp)) {
                Surface(shape = MaterialTheme.shapes.extraLarge, color = MaterialTheme.colorScheme.primaryContainer, contentColor = MaterialTheme.colorScheme.onPrimaryContainer) {
                    Text(number.toString(), style = MaterialTheme.typography.titleSmall, fontWeight = FontWeight.ExtraBold, modifier = Modifier.padding(horizontal = 10.dp, vertical = 4.dp))
                }
                Text(title, style = MaterialTheme.typography.titleLarge, fontWeight = FontWeight.ExtraBold)
            }
            content()
        }
    }
}

@Composable
private fun Form(ui: AddCustomerUiState, vm: AddCustomerViewModel) {
    StepCard(1, "Customer") {
        OutlinedTextField(ui.tenant, vm::onTenant, label = { Text("Name") }, singleLine = true, modifier = Modifier.fillMaxWidth())
        OutlinedTextField(
            ui.phone, vm::onPhone, label = { Text("Phone") }, singleLine = true,
            keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Phone),
            modifier = Modifier.fillMaxWidth(),
        )
    }
    StepCard(2, "Apartment and unit") {
        when (val a = ui.apartments) {
            Loadable.Loading -> LoadingSpinner()
            is Loadable.Failed -> Text(a.message, color = MaterialTheme.colorScheme.error)
            is Loadable.Ready -> if (a.value.isEmpty()) {
                Text("No apartments yet. Create one on the Apartments page first.", color = MaterialTheme.colorScheme.onSurfaceVariant)
            } else {
                ChipRow(horizontalPadding = 0.dp) {
                    a.value.forEach { apt ->
                        FilterChip(selected = apt.id == ui.apartmentId, onClick = { vm.onApartment(apt.id) }, label = { ChipLabel(apt.name) })
                    }
                }
            }
        }
        OutlinedTextField(
            ui.unit, vm::onUnit, label = { Text("Unit (flat), for example B4") }, singleLine = true,
            keyboardOptions = KeyboardOptions(capitalization = KeyboardCapitalization.Characters),
            modifier = Modifier.fillMaxWidth(),
        )
    }
    StepCard(3, "Package") {
        when (val p = ui.packages) {
            Loadable.Loading -> if (ui.apartmentId == null) {
                Text("Choose the apartment first.", color = MaterialTheme.colorScheme.onSurfaceVariant)
            } else {
                LoadingSpinner()
            }
            is Loadable.Failed -> Text(p.message, color = MaterialTheme.colorScheme.error)
            is Loadable.Ready -> if (p.value.isEmpty()) {
                Text("No packages for this router.", color = MaterialTheme.colorScheme.onSurfaceVariant)
            } else {
                p.value.forEach { pk -> PackageCard(pk, selected = pk.id == ui.packageId, onClick = { vm.onPackage(pk.id) }) }
            }
        }
    }
    if (ui.apartment != null) {
        val rs = ui.apartmentRouters
        StepCard(4, "Router") {
            when {
                rs.isEmpty() -> Text(
                    "This apartment has no router yet. Link one on its page first.",
                    color = MaterialTheme.colorScheme.error,
                    style = MaterialTheme.typography.bodyMedium,
                )
                // One router: chosen automatically.
                rs.size == 1 -> Text("${rs.first().label} (the only router of this apartment)", style = MaterialTheme.typography.bodyLarge)
                else -> ChipRow(horizontalPadding = 0.dp) {
                    rs.forEach { r ->
                        FilterChip(
                            selected = r.key == ui.routerId,
                            onClick = { vm.setRouter(r.key) },
                            leadingIcon = { StatusDot(r.online, size = 10.dp) },
                            label = { ChipLabel(r.label) },
                        )
                    }
                }
            }
            if (ui.routers.firstOrNull { it.key == ui.routerId }?.online == false) {
                Text(
                    "This router is offline. The customer is saved and set up on the router as soon as it is back.",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
        }
    }
    StepCard(5, "Device MAC") {
        val clipboard = LocalClipboardManager.current
        OutlinedTextField(
            ui.macInput, vm::onMac, label = { Text("MAC address") }, singleLine = true,
            isError = ui.macError != null || ui.macOwner != null || ui.macOther != null,
            supportingText = {
                Text(
                    ui.macError ?: if (ui.macValid) "Valid MAC" else "Paste it, type it, or pick a device below.",
                    maxLines = 2,
                )
            },
            textStyle = MaterialTheme.typography.bodyLarge.copy(fontFamily = FontFamily.Monospace),
            trailingIcon = { TextButton(onClick = { clipboard.getText()?.text?.let(vm::onMac) }) { Text("Paste", maxLines = 1, softWrap = false) } },
            modifier = Modifier.fillMaxWidth(),
        )
        ui.macOwner?.let { o ->
            Text(macConflictText(o), style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.error)
        }
        ui.macOther?.let { o ->
            Text(
                "${o.text}. A MAC of one apartment is never set up in another. Move that customer from the New devices page, or use another device.",
                style = MaterialTheme.typography.bodyMedium,
                color = MaterialTheme.colorScheme.error,
            )
        }
        Row(verticalAlignment = Alignment.CenterVertically) {
            Text("Devices on this router now", style = MaterialTheme.typography.titleSmall, fontWeight = FontWeight.Bold, modifier = Modifier.weight(1f))
            IconButton(onClick = vm::refreshDevices) { Icon(Icons.Default.Refresh, contentDescription = "Refresh devices") }
        }
        OutlinedTextField(
            ui.deviceQuery, vm::onDeviceQuery, singleLine = true,
            placeholder = { Text("Search MAC, vendor, host or IP", maxLines = 1) },
            leadingIcon = { Icon(Icons.Default.Search, contentDescription = null) },
            modifier = Modifier.fillMaxWidth(),
        )
        when (val d = ui.devices) {
            Loadable.Loading -> LoadingSpinner()
            is Loadable.Failed -> Text(d.message, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall)
            is Loadable.Ready -> {
                val shown = ui.shownDevices
                if (shown.isEmpty()) {
                    Text(
                        if (d.value.isEmpty()) "No unregistered devices are online. Plug the ONT in, or paste its MAC above." else "No device matches.",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                } else {
                    shown.take(30).forEach { c -> DeviceCard(c, selected = c.mac.equals(ui.macInput.trim(), ignoreCase = true), onClick = { vm.pickDevice(c) }) }
                }
            }
        }
    }
    Card(colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceContainerHigh), modifier = Modifier.fillMaxWidth()) {
        Row(Modifier.padding(16.dp), verticalAlignment = Alignment.CenterVertically) {
            Column(Modifier.weight(1f).padding(end = 12.dp)) {
                Text("Activate now", style = MaterialTheme.typography.titleMedium, fontWeight = FontWeight.Bold)
                Text(
                    "Starts the package straight away. Off: the customer lands on the pay page until paid.",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
            Switch(checked = ui.activate, onCheckedChange = vm::onActivate)
        }
    }
    ui.error?.let { Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodyMedium) }
}

@Composable
private fun PackageCard(p: ServicePackage, selected: Boolean, onClick: () -> Unit) {
    val scheme = MaterialTheme.colorScheme
    Card(
        onClick = onClick,
        modifier = Modifier.fillMaxWidth(),
        colors = CardDefaults.cardColors(
            containerColor = if (selected) scheme.primaryContainer else scheme.surfaceContainerHighest,
            contentColor = if (selected) scheme.onPrimaryContainer else scheme.onSurface,
        ),
        border = if (selected) BorderStroke(2.dp, scheme.primary) else null,
    ) {
        Row(Modifier.padding(14.dp).fillMaxWidth(), verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(12.dp)) {
            Column(Modifier.weight(1f)) {
                Text(p.name, style = MaterialTheme.typography.titleMedium, fontWeight = FontWeight.ExtraBold, maxLines = 2, overflow = TextOverflow.Ellipsis)
                Text("${p.speedMbps} Mbps  |  ${formatDuration(p.durationMin)}", style = MaterialTheme.typography.bodyMedium)
            }
            Text(formatKes(p.priceKes), style = MaterialTheme.typography.titleMedium, fontWeight = FontWeight.ExtraBold, maxLines = 1, softWrap = false)
            if (selected) Icon(Icons.Default.Check, contentDescription = "Selected")
        }
    }
}

@Composable
private fun DeviceCard(c: Client, selected: Boolean, onClick: () -> Unit) {
    val scheme = MaterialTheme.colorScheme
    Card(
        onClick = onClick,
        modifier = Modifier.fillMaxWidth(),
        colors = CardDefaults.cardColors(
            containerColor = if (selected) scheme.tertiaryContainer else scheme.surfaceContainerHighest,
            contentColor = if (selected) scheme.onTertiaryContainer else scheme.onSurface,
        ),
        border = if (selected) BorderStroke(2.dp, scheme.tertiary) else null,
    ) {
        Row(Modifier.padding(12.dp).fillMaxWidth(), verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(10.dp)) {
            StatusDot(c.online, size = 10.dp)
            Column(Modifier.weight(1f)) {
                Text(c.mac, style = MaterialTheme.typography.bodyLarge.copy(fontFamily = FontFamily.Monospace), fontWeight = FontWeight.Bold, maxLines = 1)
                Text(
                    listOf(c.vendor, c.hostName, c.ip).filter { it.isNotBlank() }.joinToString("  |  ").ifBlank { "Unknown device" },
                    style = MaterialTheme.typography.bodySmall,
                    maxLines = 2,
                    overflow = TextOverflow.Ellipsis,
                )
            }
            if (selected) Icon(Icons.Default.Check, contentDescription = "Selected")
        }
    }
}

@Composable
private fun ResultCard(a: AddedCustomer, onOpen: () -> Unit, onAnother: () -> Unit, onRecheck: () -> Unit, onDone: () -> Unit) {
    val scheme = MaterialTheme.colorScheme
    val (container, content) = when (a.status) {
        ProvisionStatus.Provisioned -> ke.co.ftw.ftnadmin.ui.theme.AppTheme.extended.let { it.successContainer to it.onSuccessContainer }
        ProvisionStatus.Queued, ProvisionStatus.Working -> ke.co.ftw.ftnadmin.ui.theme.AppTheme.extended.let { it.warningContainer to it.onWarningContainer }
        is ProvisionStatus.Failed -> scheme.errorContainer to scheme.onErrorContainer
    }
    Card(Modifier.fillMaxWidth(), colors = CardDefaults.cardColors(containerColor = container, contentColor = content)) {
        Column(Modifier.padding(20.dp), verticalArrangement = Arrangement.spacedBy(10.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                when (a.status) {
                    ProvisionStatus.Provisioned -> Icon(Icons.Default.Check, contentDescription = null)
                    ProvisionStatus.Working -> LoadingSpinner(Modifier.padding(4.dp))
                    else -> Unit
                }
                Text(a.status.label, style = MaterialTheme.typography.titleLarge, fontWeight = FontWeight.ExtraBold, modifier = Modifier.weight(1f))
            }
            if (a.status is ProvisionStatus.Failed) Text(a.status.reason, style = MaterialTheme.typography.bodyMedium)
            if (a.status == ProvisionStatus.Queued) {
                Text(
                    "The router is offline or busy. The account is saved and will be set up on the router by itself.",
                    style = MaterialTheme.typography.bodyMedium,
                )
            }
        }
    }
    Card(Modifier.fillMaxWidth(), colors = CardDefaults.cardColors(containerColor = scheme.surfaceContainerHigh)) {
        Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(6.dp)) {
            Text("Customer created", style = MaterialTheme.typography.titleLarge, fontWeight = FontWeight.ExtraBold)
            LabeledValue("Apartment", a.account.apartment?.name.orEmpty())
            LabeledValue("Unit", a.account.unitLabel)
            if (a.account.tenant.isNotBlank()) LabeledValue("Name", a.account.tenant)
            LabeledValue("Code", a.account.code)
            LabeledValue("Router", a.account.routerLabel)
            LabeledValue("MAC", a.account.mac)
            a.account.pkg?.let { LabeledValue("Package", it.name) }
            a.account.router?.let { r ->
                LabeledValue("Lease", r.leaseStatus.replaceFirstChar { it.uppercase() })
                if (r.suspendedOnRouter) StatusChip("Waiting for payment", Tone.Warn)
            }
        }
    }
    Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
        Button(onClick = onOpen, modifier = Modifier.fillMaxWidth()) { Text("Open customer", maxLines = 1) }
        if (a.status == ProvisionStatus.Queued || a.status is ProvisionStatus.Failed) {
            OutlinedButton(onClick = onRecheck, modifier = Modifier.fillMaxWidth()) { Text("Check again", maxLines = 1) }
        }
        OutlinedButton(onClick = onAnother, modifier = Modifier.fillMaxWidth()) { Text("Add another customer", maxLines = 1) }
        TextButton(onClick = onDone, modifier = Modifier.fillMaxWidth()) { Text("Done", maxLines = 1) }
    }
}
