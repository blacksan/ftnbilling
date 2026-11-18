package ke.co.ftw.ftnadmin.ui.more

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import ke.co.ftw.ftnadmin.ui.common.AppTopBar
import ke.co.ftw.ftnadmin.ui.common.LocalContainer
import ke.co.ftw.ftnadmin.ui.common.SectionCard
import ke.co.ftw.ftnadmin.ui.common.vmFactory

@Composable
fun AddRouterScreen(onBack: () -> Unit, onOpenSetup: (String) -> Unit) {
    val container = LocalContainer.current
    val vm: AddRouterViewModel = viewModel(
        factory = vmFactory { AddRouterViewModel(container.routerLive, onCreated = { container.routerContext.refresh() }) },
    )
    val ui by vm.state.collectAsStateWithLifecycle()
    Scaffold(
        containerColor = MaterialTheme.colorScheme.background,
        topBar = { AppTopBar("Add router", onBack = onBack) },
    ) { inner ->
        Column(
            Modifier.padding(inner).fillMaxSize().imePadding().verticalScroll(rememberScrollState()).padding(16.dp),
            verticalArrangement = Arrangement.spacedBy(16.dp),
        ) {
            val created = ui.created
            if (created != null) {
                Card(Modifier.fillMaxWidth(), colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.primaryContainer, contentColor = MaterialTheme.colorScheme.onPrimaryContainer)) {
                    Column(Modifier.padding(20.dp), verticalArrangement = Arrangement.spacedBy(6.dp)) {
                        Text("Router added", style = MaterialTheme.typography.headlineSmall, fontWeight = FontWeight.ExtraBold)
                        Text(created.router.label.ifBlank { ui.name }, style = MaterialTheme.typography.titleMedium, fontWeight = FontWeight.Bold)
                        Text(
                            "Paste the command below in the router terminal. The server does the rest by itself.",
                            style = MaterialTheme.typography.bodyMedium,
                        )
                    }
                }
                SectionCard("Join command") {
                    Text(
                        "Shown once. It is never saved on this phone.",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                    JoinCommandBlock(created.join)
                }
                val id = created.router.key
                if (id.isNotBlank()) {
                    Button(onClick = { onOpenSetup(id) }, modifier = Modifier.fillMaxWidth()) { Text("Watch setup progress", maxLines = 1) }
                }
                OutlinedButton(onClick = onBack, modifier = Modifier.fillMaxWidth()) { Text("Done", maxLines = 1) }
            } else {
                SectionCard("New router") {
                    OutlinedTextField(ui.name, vm::onName, label = { Text("Router name") }, singleLine = true, modifier = Modifier.fillMaxWidth())
                    Row(Modifier.fillMaxWidth(), verticalAlignment = Alignment.CenterVertically) {
                        Column(Modifier.weight(1f).padding(end = 12.dp)) {
                            Text("Already serving customers?", style = MaterialTheme.typography.titleMedium, fontWeight = FontWeight.Bold)
                            Text(
                                "Turn on for a live router that customers use today. It is migrated safely after it joins.",
                                style = MaterialTheme.typography.bodySmall,
                                color = MaterialTheme.colorScheme.onSurfaceVariant,
                            )
                        }
                        Switch(checked = ui.serving, onCheckedChange = vm::onServing)
                    }
                    TextButton(onClick = vm::toggleAdvanced) { Text(if (ui.advanced) "Hide advanced" else "Advanced settings", maxLines = 1) }
                    if (ui.advanced) {
                        OutlinedTextField(ui.wanInterface, vm::onWan, label = { Text("WAN port") }, singleLine = true, modifier = Modifier.fillMaxWidth())
                        OutlinedTextField(ui.oltInterface, vm::onOlt, label = { Text("OLT port") }, singleLine = true, modifier = Modifier.fillMaxWidth())
                        OutlinedTextField(
                            ui.vlan, vm::onVlan, label = { Text("VLAN") }, singleLine = true,
                            keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Number),
                            modifier = Modifier.fillMaxWidth(),
                        )
                        OutlinedTextField(ui.subnet, vm::onSubnet, label = { Text("Customer subnet") }, singleLine = true, modifier = Modifier.fillMaxWidth())
                        OutlinedTextField(
                            ui.wanCap, vm::onWanCap, label = { Text("WAN capacity (Mbps)") }, singleLine = true,
                            keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Number),
                            modifier = Modifier.fillMaxWidth(),
                        )
                        OutlinedTextField(
                            ui.supportPhone, vm::onSupportPhone, label = { Text("Support phone") }, singleLine = true,
                            keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Phone),
                            modifier = Modifier.fillMaxWidth(),
                        )
                    }
                    ui.error?.let { Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodyMedium) }
                    Button(onClick = vm::submit, enabled = ui.canSubmit, modifier = Modifier.fillMaxWidth()) {
                        Text(if (ui.busy) "Adding..." else "Add router", maxLines = 1)
                    }
                }
            }
        }
    }
}
