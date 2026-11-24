package ke.co.ftw.ftnadmin.ui.onu

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material3.Button
import androidx.compose.material3.FilterChip
import androidx.compose.ui.text.input.KeyboardType
import ke.co.ftw.ftnadmin.ui.common.ChipLabel
import ke.co.ftw.ftnadmin.ui.common.ChipRow
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.ModalBottomSheet
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.rememberModalBottomSheetState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.compose.ui.Modifier
import androidx.compose.ui.res.stringResource
import androidx.compose.foundation.layout.imePadding
import androidx.compose.material3.LocalTextStyle
import androidx.compose.material3.pulltorefresh.PullToRefreshBox
import androidx.compose.ui.text.input.KeyboardCapitalization
import ke.co.ftw.ftnadmin.R
import ke.co.ftw.ftnadmin.ui.theme.GeistMono
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import ke.co.ftw.ftnadmin.core.network.UserInfo
import ke.co.ftw.ftnadmin.data.onu.RegisterItem
import ke.co.ftw.ftnadmin.ui.common.AppTopBar
import ke.co.ftw.ftnadmin.ui.common.CenteredLoading
import ke.co.ftw.ftnadmin.ui.common.ErrorState
import ke.co.ftw.ftnadmin.ui.common.LabeledValue
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.common.LocalContainer
import ke.co.ftw.ftnadmin.ui.common.SectionCard
import ke.co.ftw.ftnadmin.ui.common.StatusChip
import ke.co.ftw.ftnadmin.ui.common.Tone
import ke.co.ftw.ftnadmin.ui.common.vmFactory

/** The ONU register: every ONU the owner supplies, as a serial and WAN MAC pair, with its status. */
@OptIn(ExperimentalLayoutApi::class, ExperimentalMaterial3Api::class)
@Composable
fun RegisterScreen(user: UserInfo, onBack: () -> Unit) {
    val container = LocalContainer.current
    val vm: RegisterViewModel = viewModel(key = "onu-register", factory = vmFactory { RegisterViewModel(container.onu, container.equipment) })
    val ui by vm.state.collectAsStateWithLifecycle()
    val snack = remember { SnackbarHostState() }
    LaunchedEffect(ui.info) {
        ui.info?.let {
            snack.showSnackbar(it)
            vm.clearInfo()
        }
    }
    Scaffold(
        containerColor = MaterialTheme.colorScheme.background,
        topBar = { AppTopBar(stringResource(R.string.onu_register), onBack = onBack) },
        snackbarHost = { SnackbarHost(snack) },
    ) { inner ->
        when (val list = ui.items) {
            Loadable.Loading -> CenteredLoading(Modifier.padding(inner))
            is Loadable.Failed -> ErrorState(list.message, vm::load, Modifier.padding(inner))
            is Loadable.Ready -> PullToRefreshBox(ui.refreshing, vm::refresh, Modifier.padding(inner).fillMaxSize()) {
              LazyColumn(
                Modifier.fillMaxSize(),
                contentPadding = androidx.compose.foundation.layout.PaddingValues(16.dp),
                verticalArrangement = Arrangement.spacedBy(12.dp),
              ) {
                if (user.canWrite) item { Button(onClick = vm::openAdd, modifier = Modifier.fillMaxWidth()) { Text(stringResource(R.string.add_an_onu)) } }
                if (list.value.isEmpty()) {
                    item { Text("The register has no ONUs yet.", color = MaterialTheme.colorScheme.onSurfaceVariant, style = MaterialTheme.typography.bodyMedium) }
                }
                items(list.value, key = { it.id }) { RegisterCard(it) }
              }
            }
        }
    }
    if (ui.adding) {
        ModalBottomSheet(onDismissRequest = vm::closeAdd, sheetState = rememberModalBottomSheetState(skipPartiallyExpanded = true)) {
            Column(
                Modifier.fillMaxWidth().imePadding().verticalScroll(rememberScrollState()).padding(horizontal = 16.dp).padding(top = 8.dp, bottom = 16.dp),
                verticalArrangement = Arrangement.spacedBy(10.dp),
            ) {
                Text("Add an ONU", style = MaterialTheme.typography.titleLarge, color = MaterialTheme.colorScheme.onSurface)
                Text(
                    "Use the WAN MAC the router sees. The MAC on the label is often the LAN MAC: confirm it on the bench first.",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
                val f = ui.form
                OutlinedTextField(
                    f.serial, vm::onSerial, label = { Text(stringResource(R.string.serial_number)) }, singleLine = true,
                    textStyle = LocalTextStyle.current.copy(fontFamily = GeistMono),
                    keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Ascii, capitalization = KeyboardCapitalization.Characters),
                    isError = f.errors["serial"] != null, supportingText = { f.errors["serial"]?.let { Text(it) } }, modifier = Modifier.fillMaxWidth(),
                )
                OutlinedTextField(f.gponSerial, vm::onGpon, label = { Text(stringResource(R.string.gpon_serial)) }, supportingText = { Text(stringResource(R.string.gpon_serial_hint)) }, singleLine = true,
                    textStyle = LocalTextStyle.current.copy(fontFamily = GeistMono),
                    keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Ascii, capitalization = KeyboardCapitalization.Characters),
                    modifier = Modifier.fillMaxWidth())
                OutlinedTextField(
                    f.mac, vm::onMac, label = { Text(stringResource(R.string.wan_mac)) }, singleLine = true,
                    textStyle = LocalTextStyle.current.copy(fontFamily = GeistMono),
                    keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Ascii, capitalization = KeyboardCapitalization.Characters),
                    isError = f.errors["mac"] != null, supportingText = { f.errors["mac"]?.let { Text(it) } }, modifier = Modifier.fillMaxWidth(),
                )
                OutlinedTextField(f.model, vm::onModel, label = { Text("Model") }, singleLine = true, modifier = Modifier.fillMaxWidth())
                if (ui.olts.isNotEmpty()) {
                    Text("OLT (optional)", style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
                    FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                        ui.olts.forEach { o -> FilterChip(f.oltId == o.id, { vm.onOlt(o.id) }, label = { Text(o.name.ifBlank { o.mac }) }) }
                    }
                    if (f.oltId.isNotBlank()) {
                        OutlinedTextField(
                            f.ponPort, vm::onPonPort, label = { Text("PON port") }, singleLine = true,
                            keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Number),
                            isError = f.errors["pon_port"] != null, supportingText = { f.errors["pon_port"]?.let { Text(it) } },
                            modifier = Modifier.fillMaxWidth(),
                        )
                    }
                }
                ui.error?.let { Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodyMedium) }
                Button(onClick = vm::save, enabled = !ui.busy, modifier = Modifier.fillMaxWidth()) { Text(stringResource(if (ui.busy) R.string.saving else R.string.action_save)) }
                TextButton(onClick = vm::closeAdd, modifier = Modifier.fillMaxWidth()) { Text("Cancel") }
            }
        }
    }
}

@OptIn(ExperimentalLayoutApi::class)
@Composable
private fun RegisterCard(e: RegisterItem) {
    SectionCard(e.serial.ifBlank { stringResource(R.string.no_serial) }) {
        FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
            StatusChip(registerStatusLabel(e.status), registerTone(e.status))
        }
        LabeledValue(stringResource(R.string.wan_mac), e.mac, mono = true)
        if (e.oltId.isNotBlank()) LabeledValue("OLT", if (e.ponPort > 0) "${e.oltName.ifBlank { e.oltId }}, PON ${e.ponPort}" else e.oltName.ifBlank { e.oltId })
        if (e.gponSerial.isNotBlank()) LabeledValue(stringResource(R.string.gpon_serial_short), e.gponSerial, mono = true)
        LabeledValue("Model", e.model)
    }
}

internal fun registerStatusLabel(status: String): String = when (status) {
    "in_stock" -> "In stock"
    "installed" -> "Deployed"
    "faulty" -> "Faulty"
    "returned" -> "Returned"
    "retired" -> "Retired"
    else -> status
}

internal fun registerTone(status: String): Tone = when (status) {
    "installed" -> Tone.Good
    "faulty" -> Tone.Bad
    "returned" -> Tone.Warn
    else -> Tone.Neutral
}
