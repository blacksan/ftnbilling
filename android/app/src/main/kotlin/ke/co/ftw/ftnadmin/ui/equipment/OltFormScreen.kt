package ke.co.ftw.ftnadmin.ui.equipment

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Button
import androidx.compose.material3.Checkbox
import androidx.compose.material3.FilterChip
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import ke.co.ftw.ftnadmin.ui.common.AppTopBar
import ke.co.ftw.ftnadmin.ui.common.CenteredLoading
import ke.co.ftw.ftnadmin.ui.common.ChipLabel
import ke.co.ftw.ftnadmin.ui.common.ChipRow
import ke.co.ftw.ftnadmin.ui.common.LocalContainer
import ke.co.ftw.ftnadmin.ui.common.SectionCard
import ke.co.ftw.ftnadmin.ui.common.vmFactory

/** Add or edit an OLT. The password field is write-only: blank keeps the stored one, and it is never filled in. */
@Composable
fun OltFormScreen(oltId: String?, initialSiteId: String?, onBack: () -> Unit, onSaved: () -> Unit) {
    val container = LocalContainer.current
    val vm: OltFormViewModel = viewModel(
        key = "olt-form-${oltId.orEmpty()}",
        factory = vmFactory { OltFormViewModel(oltId, container.equipment, container.sites, container.apartments, initialSiteId) },
    )
    val ui by vm.state.collectAsStateWithLifecycle()
    LaunchedEffect(ui.saved) { if (ui.saved != null) onSaved() }
    Scaffold(
        containerColor = MaterialTheme.colorScheme.background,
        topBar = { AppTopBar(if (oltId == null) "Add an OLT" else "Edit OLT", onBack = onBack) },
    ) { inner ->
        if (ui.loading) {
            CenteredLoading(Modifier.padding(inner))
            return@Scaffold
        }
        val f = ui.form
        Column(
            Modifier.padding(inner).fillMaxSize().verticalScroll(rememberScrollState()).padding(16.dp),
            verticalArrangement = Arrangement.spacedBy(12.dp),
        ) {
            SectionCard("The OLT") {
                Field(f.name, vm::onName, "Name", f.errors["name"])
                Field(f.mac, vm::onMac, "MAC address", f.errors["mac"])
                Field(f.model, vm::onModel, "Model", null)
                Field(f.serial, vm::onSerial, "Serial", null)
            }
            SectionCard("Router") {
                Text("The router it connects to", style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
                ChipRow(horizontalPadding = 0.dp) {
                    ui.routers.forEach { r -> FilterChip(f.siteId == r.key, { vm.onRouter(r.key) }, label = { ChipLabel(r.label) }) }
                }
                f.errors["site_id"]?.let { Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall) }
                Field(f.routerPort, vm::onRouterPort, "Router port (like ether5)", f.errors["router_port"])
            }
            SectionCard("PON ports") {
                Field(f.ponPorts, vm::onPonPorts, "Number of PON ports", f.errors["pon_ports"], KeyboardType.Number)
                if (ui.ports.isNotEmpty() && ui.apartments.isEmpty()) {
                    Text(
                        "This router serves no apartment yet, so a port cannot be given one.",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
                ui.ports.forEach { port ->
                    Text("PON $port serves", style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
                    ChipRow(horizontalPadding = 0.dp) {
                        ui.apartments.forEach { a ->
                            FilterChip(a.id in f.ponApartments[port].orEmpty(), { vm.toggleApartment(port, a.id) }, label = { ChipLabel(a.name) })
                        }
                    }
                }
                f.errors["pon_apartments"]?.let { Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall) }
            }
            SectionCard("Management") {
                Field(f.mgmtIp, vm::onMgmtIp, "Management address", f.errors["mgmt_ip"])
                Field(f.mgmtUser, vm::onMgmtUser, "Username", null)
                OutlinedTextField(
                    f.mgmtPass, vm::onMgmtPass,
                    label = { Text(if (ui.hadPass) "New password (blank keeps the stored one)" else "Password") },
                    singleLine = true,
                    visualTransformation = PasswordVisualTransformation(),
                    keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Password),
                    isError = f.errors["mgmt_pass"] != null,
                    supportingText = { f.errors["mgmt_pass"]?.let { Text(it) } },
                    modifier = Modifier.fillMaxWidth(),
                )
                if (ui.hadPass) {
                    Row(verticalAlignment = Alignment.CenterVertically) {
                        Checkbox(f.clearMgmtPass, vm::onClearPass)
                        Text("Remove the stored password", style = MaterialTheme.typography.bodyMedium)
                    }
                }
            }
            ui.error?.let { Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodyMedium) }
            Button(onClick = vm::save, enabled = !ui.busy, modifier = Modifier.fillMaxWidth()) { Text(if (ui.busy) "Saving..." else "Save") }
        }
    }
}

@Composable
private fun Field(value: String, onChange: (String) -> Unit, label: String, error: String?, type: KeyboardType = KeyboardType.Text) {
    OutlinedTextField(
        value, onChange, label = { Text(label) }, singleLine = true,
        keyboardOptions = KeyboardOptions(keyboardType = type),
        isError = error != null, supportingText = { error?.let { Text(it) } }, modifier = Modifier.fillMaxWidth(),
    )
}
