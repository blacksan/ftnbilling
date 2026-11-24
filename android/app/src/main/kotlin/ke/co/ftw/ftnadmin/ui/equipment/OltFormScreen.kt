package ke.co.ftw.ftnadmin.ui.equipment

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Button
import androidx.compose.material3.LocalTextStyle
import androidx.compose.material3.Surface
import androidx.compose.material3.FilterChip
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.input.KeyboardCapitalization
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import ke.co.ftw.ftnadmin.ui.common.AppTopBar
import ke.co.ftw.ftnadmin.ui.common.CenteredLoading
import ke.co.ftw.ftnadmin.R
import ke.co.ftw.ftnadmin.ui.common.CheckRow
import ke.co.ftw.ftnadmin.ui.theme.GeistMono
import ke.co.ftw.ftnadmin.ui.common.LocalContainer
import ke.co.ftw.ftnadmin.ui.common.SectionCard
import ke.co.ftw.ftnadmin.ui.common.vmFactory

/** Add or edit an OLT. The password field is write-only: blank keeps the stored one, and it is never filled in. */
@OptIn(ExperimentalLayoutApi::class)
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
        topBar = { AppTopBar(stringResource(if (oltId == null) R.string.add_an_olt else R.string.edit_olt), onBack = onBack) },
        // Save sits above the navigation bar and the keyboard, so it is always reachable (same as Add customer).
        bottomBar = {
            if (!ui.loading) {
                Surface(tonalElevation = 3.dp) {
                    Column(Modifier.fillMaxWidth().navigationBarsPadding().imePadding().padding(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                        ui.error?.let { Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodyMedium) }
                        Button(onClick = vm::save, enabled = !ui.busy, modifier = Modifier.fillMaxWidth()) {
                            Text(stringResource(if (ui.busy) R.string.saving else R.string.action_save))
                        }
                    }
                }
            }
        },
    ) { inner ->
        if (ui.loading) {
            CenteredLoading(Modifier.padding(inner))
            return@Scaffold
        }
        val f = ui.form
        Column(
            Modifier.padding(inner).fillMaxSize().imePadding().verticalScroll(rememberScrollState()).padding(16.dp),
            verticalArrangement = Arrangement.spacedBy(12.dp),
        ) {
            SectionCard(stringResource(R.string.the_olt)) {
                Field(f.name, vm::onName, stringResource(R.string.label_name), f.errors["name"])
                Field(f.mac, vm::onMac, stringResource(R.string.mac_address), f.errors["mac"], mono = true, caps = true)
                Field(f.model, vm::onModel, stringResource(R.string.model), null)
                Field(f.serial, vm::onSerial, stringResource(R.string.serial), null, mono = true, caps = true)
            }
            SectionCard(stringResource(R.string.label_router)) {
                Text(stringResource(R.string.olt_router_hint), style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
                FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    ui.routers.forEach { r -> FilterChip(f.siteId == r.key, { vm.onRouter(r.key) }, label = { Text(r.label) }) }
                }
                f.errors["site_id"]?.let { Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodyMedium) }
                Field(f.routerPort, vm::onRouterPort, stringResource(R.string.router_port), f.errors["router_port"], hint = stringResource(R.string.router_port_hint), mono = true)
            }
            SectionCard(stringResource(R.string.pon_ports)) {
                Field(f.ponPorts, vm::onPonPorts, stringResource(R.string.pon_port_count), f.errors["pon_ports"], KeyboardType.Number)
                if (ui.ports.isNotEmpty() && ui.apartments.isEmpty()) {
                    Text(
                        stringResource(R.string.no_apartment_for_port),
                        style = MaterialTheme.typography.bodyMedium,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
                ui.ports.forEach { port ->
                    Text(stringResource(R.string.pon_serves, port), style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
                    // Several apartments can be ticked, so they wrap in view instead of scrolling off to the side.
                    FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                        ui.apartments.forEach { a ->
                            FilterChip(a.id in f.ponApartments[port].orEmpty(), { vm.toggleApartment(port, a.id) }, label = { Text(a.name) })
                        }
                    }
                }
                f.errors["pon_apartments"]?.let { Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodyMedium) }
            }
            SectionCard(stringResource(R.string.management)) {
                Field(f.mgmtIp, vm::onMgmtIp, stringResource(R.string.management_address), f.errors["mgmt_ip"], mono = true)
                Field(f.mgmtUser, vm::onMgmtUser, stringResource(R.string.username), null)
                OutlinedTextField(
                    f.mgmtPass, vm::onMgmtPass,
                    label = { Text(stringResource(if (ui.hadPass) R.string.new_password else R.string.password)) },
                    singleLine = true,
                    visualTransformation = PasswordVisualTransformation(),
                    keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Password),
                    isError = f.errors["mgmt_pass"] != null,
                    supportingText = (f.errors["mgmt_pass"] ?: if (ui.hadPass) stringResource(R.string.password_blank_keeps) else null)?.let { m -> { Text(m) } },
                    modifier = Modifier.fillMaxWidth(),
                )
                if (ui.hadPass) {
                    CheckRow(f.clearMgmtPass, vm::onClearPass, stringResource(R.string.remove_stored_password))
                }
            }
        }
    }
}

/** A text field. [hint] shows under it when there is no error; the label stays short. [mono] is for MACs, serials and IPs. */
@Composable
private fun Field(
    value: String,
    onChange: (String) -> Unit,
    label: String,
    error: String?,
    type: KeyboardType = KeyboardType.Text,
    hint: String? = null,
    mono: Boolean = false,
    caps: Boolean = false,
) {
    val support = error ?: hint
    OutlinedTextField(
        value, onChange, label = { Text(label) }, singleLine = true,
        textStyle = if (mono) LocalTextStyle.current.copy(fontFamily = GeistMono) else LocalTextStyle.current,
        keyboardOptions = KeyboardOptions(
            keyboardType = if (caps && type == KeyboardType.Text) KeyboardType.Ascii else type,
            capitalization = if (caps) KeyboardCapitalization.Characters else KeyboardCapitalization.None,
        ),
        isError = error != null,
        supportingText = support?.let { m -> { Text(m) } },
        modifier = Modifier.fillMaxWidth(),
    )
}
