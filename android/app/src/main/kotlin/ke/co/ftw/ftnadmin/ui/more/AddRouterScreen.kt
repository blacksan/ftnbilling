package ke.co.ftw.ftnadmin.ui.more

import ke.co.ftw.ftnadmin.domain.customerPortLine
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import ke.co.ftw.ftnadmin.ui.common.AppTopBar
import ke.co.ftw.ftnadmin.ui.common.CenteredLoading
import ke.co.ftw.ftnadmin.ui.common.ErrorState
import ke.co.ftw.ftnadmin.ui.common.LocalContainer
import ke.co.ftw.ftnadmin.ui.common.SectionCard
import ke.co.ftw.ftnadmin.ui.common.vmFactory
import ke.co.ftw.ftnadmin.ui.routers.setup.BootstrapCommandBlock

@Composable
fun AddRouterScreen(onBack: () -> Unit, onOpenRouter: (String) -> Unit) {
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
                        Text(created.router.label.ifBlank { ui.form.name }, style = MaterialTheme.typography.titleMedium, fontWeight = FontWeight.Bold)
                        Text(
                            "Paste the line below in the terminal of a blank router. The router erases itself and the server does the rest.",
                            style = MaterialTheme.typography.bodyMedium,
                        )
                    }
                }
                SectionCard("Bootstrap line") {
                    Text(
                        "Shown once. It is never saved on this phone.",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                    BootstrapCommandBlock(created.join)
                }
                val id = created.router.key
                if (id.isNotBlank()) {
                    Button(onClick = { onOpenRouter(id) }, modifier = Modifier.fillMaxWidth()) { Text("Watch setup progress", maxLines = 1) }
                }
                OutlinedButton(onClick = onBack, modifier = Modifier.fillMaxWidth()) { Text("Done", maxLines = 1) }
            } else {
                SectionCard("New router") {
                    OutlinedTextField(
                        ui.form.name, { v -> vm.edit(RouterField.NAME) { it.copy(name = v) } },
                        label = { Text("Router name") }, singleLine = true,
                        isError = ui.fieldErrors[RouterField.NAME] != null,
                        supportingText = ui.fieldErrors[RouterField.NAME]?.let { t -> { Text(t) } },
                        modifier = Modifier.fillMaxWidth(),
                    )
                    TextButton(onClick = vm::toggleAdvanced) { Text(if (ui.advanced) "Hide settings" else "Network settings", maxLines = 1) }
                }
                if (ui.advanced) RouterFormFields(ui.form, ui.fieldErrors, vm::edit, includeName = false)
                ui.error?.let { Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodyMedium) }
                Button(onClick = vm::submit, enabled = ui.canSubmit, modifier = Modifier.fillMaxWidth()) {
                    Text(if (ui.busy) "Adding..." else "Add router", maxLines = 1)
                }
            }
        }
    }
}

@Composable
fun EditRouterScreen(routerId: String, onBack: () -> Unit) {
    val container = LocalContainer.current
    val vm: EditRouterViewModel = viewModel(
        key = "edit-router-$routerId",
        factory = vmFactory {
            EditRouterViewModel(routerId, container.sites, container.routerLive, onSaved = { container.routerContext.refresh() })
        },
    )
    val ui by vm.state.collectAsStateWithLifecycle()
    androidx.compose.runtime.LaunchedEffect(ui.saved) { if (ui.saved) onBack() }
    Scaffold(
        containerColor = MaterialTheme.colorScheme.background,
        topBar = { AppTopBar("Router settings", onBack = onBack) },
    ) { inner ->
        val form = ui.form
        when {
            form == null && ui.loadError != null -> ErrorState(ui.loadError.orEmpty(), vm::load, Modifier.padding(inner))
            form == null -> CenteredLoading(Modifier.padding(inner))
            else -> Column(
                Modifier.padding(inner).fillMaxSize().imePadding().verticalScroll(rememberScrollState()).padding(16.dp),
                verticalArrangement = Arrangement.spacedBy(16.dp),
            ) {
                customerPortLine(ui.customerPort)?.let {
                    Text(it, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
                }
                RouterFormFields(form, ui.fieldErrors, vm::edit, ports = ui.ports)
                ui.error?.let { Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodyMedium) }
                Button(onClick = vm::save, enabled = ui.canSave, modifier = Modifier.fillMaxWidth()) {
                    Text(if (ui.busy) "Saving..." else "Save", maxLines = 1)
                }
                if (ui.confirmOlt) {
                    androidx.compose.material3.AlertDialog(
                        onDismissRequest = vm::cancelConfirm,
                        title = { Text("Change the OLT setting?") },
                        text = { Text(RouterFormCopy.OLT_CONFIRM + " The server moves the port and the VLAN on the router by itself as soon as you confirm.") },
                        confirmButton = { TextButton(onClick = vm::confirmSave) { Text("Change it") } },
                        dismissButton = { TextButton(onClick = vm::cancelConfirm) { Text("Cancel") } },
                    )
                }
            }
        }
    }
}
