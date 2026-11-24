package ke.co.ftw.ftnadmin.ui.onu

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.foundation.layout.Column
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.ModalBottomSheet
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Scaffold
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.rememberModalBottomSheetState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.res.stringResource
import androidx.compose.foundation.layout.imePadding
import androidx.compose.material3.pulltorefresh.PullToRefreshBox
import ke.co.ftw.ftnadmin.R
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import ke.co.ftw.ftnadmin.core.network.UserInfo
import ke.co.ftw.ftnadmin.data.onu.Onu
import ke.co.ftw.ftnadmin.ui.common.AccountPicker
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

/** The ONUs of one router: who they are, which house each is linked to, link and unlink, and the register warnings. */
@OptIn(ExperimentalLayoutApi::class, ExperimentalMaterial3Api::class)
@Composable
fun OnusScreen(
    siteId: String,
    user: UserInfo,
    onBack: () -> Unit,
    onOpenAccount: (String) -> Unit,
    onRegister: () -> Unit,
) {
    val container = LocalContainer.current
    val vm: OnusViewModel = viewModel(key = "onus-$siteId", factory = vmFactory { OnusViewModel(siteId, container.onu, container.accounts) })
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
        topBar = { AppTopBar(stringResource(R.string.onus), onBack = onBack) },
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
                item { OutlinedButton(onClick = onRegister, modifier = Modifier.fillMaxWidth()) { Text(stringResource(R.string.onu_register_link)) } }
                ui.error?.let { item { Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodyMedium) } }
                if (list.value.isEmpty()) {
                    item {
                        Text(
                            "No ONU has reported yet. Once the OLT gives the TR-069 address to the ONUs, each one shows here within a few minutes.",
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                            style = MaterialTheme.typography.bodyMedium,
                        )
                    }
                }
                items(list.value, key = { it.id }) { onu ->
                    OnuCard(onu, user.canWrite, ui.busy, vm, onOpenAccount)
                }
              }
            }
        }
    }
    ui.linking?.let { onu ->
        ModalBottomSheet(onDismissRequest = vm::cancelLink, sheetState = rememberModalBottomSheetState(skipPartiallyExpanded = true)) {
            Column(
                Modifier.fillMaxWidth().imePadding().verticalScroll(rememberScrollState()).padding(horizontal = 16.dp).padding(top = 8.dp, bottom = 16.dp),
                verticalArrangement = Arrangement.spacedBy(10.dp),
            ) {
                Text(stringResource(R.string.link_onu_to_house, onu.serial), style = MaterialTheme.typography.titleLarge, color = MaterialTheme.colorScheme.onSurface)
                var picked by remember { mutableStateOf<ke.co.ftw.ftnadmin.domain.Account?>(null) }
                AccountPicker(selectedId = picked?.id, onSelect = { picked = it }, search = vm::searchHouses)
                Button(
                    onClick = { picked?.let(vm::link) },
                    enabled = picked != null && !ui.busy,
                    modifier = Modifier.fillMaxWidth(),
                ) { Text("Link") }
            }
        }
    }
}

@OptIn(ExperimentalLayoutApi::class)
@Composable
private fun OnuCard(onu: Onu, canWrite: Boolean, busy: Boolean, vm: OnusViewModel, onOpenAccount: (String) -> Unit) {
    var confirmUnlink by remember { mutableStateOf(false) }
    SectionCard(onu.title) {
        FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
            StatusChip(stringResource(if (onu.online) R.string.state_online else R.string.silent), if (onu.online) Tone.Good else Tone.Warn)
            if (!onu.inInventory) StatusChip("Not in the register", Tone.Neutral)
            if (onu.isQuarantined) StatusChip("Held back", Tone.Bad)
            if (onu.blocked) StatusChip("Blocked", Tone.Bad)
            if (onu.sharedLoginOnly) StatusChip("Shared login only", Tone.Neutral)
            if (onu.returned) StatusChip("Returned", Tone.Neutral)
        }
        onu.quarantine?.let { q ->
            Text(
                "ONU ${onu.serial} checked in from an unexpected place (${q.reason}). It gets no settings and no tasks until you decide.",
                style = MaterialTheme.typography.bodyMedium,
                color = MaterialTheme.colorScheme.error,
            )
            if (canWrite) {
                FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                    Button(onClick = { vm.approve(onu) }, enabled = !busy) { Text("Approve") }
                    OutlinedButton(onClick = { vm.block(onu) }, enabled = !busy) { Text("Block") }
                }
            }
        }
        if (onu.blocked && !onu.isQuarantined && canWrite) {
            OutlinedButton(onClick = { vm.unblock(onu) }, enabled = !busy) { Text("Unblock") }
        }
        LabeledValue(stringResource(R.string.serial), onu.serial, mono = true)
        LabeledValue(stringResource(R.string.management_ip), onu.mgmtIp, mono = true)
        LabeledValue("MAC", onu.macs.joinToString(", "), mono = true)
        LabeledValue(stringResource(R.string.house), if (onu.isLinked) "${onu.unit} (${onu.accountCode})" else stringResource(R.string.not_linked))
        onu.wifi.forEach { w -> LabeledValue("${w.band} GHz", w.ssid) }
        onu.warnings.forEach { Text(it, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.error) }
        FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
            if (onu.isLinked) OutlinedButton(onClick = { onOpenAccount(onu.accountId) }) { Text("Open house") }
            if (canWrite) {
                if (onu.isLinked) {
                    OutlinedButton(onClick = { confirmUnlink = true }, enabled = !busy) { Text("Unlink") }
                    onu.useMacs.firstOrNull()?.let { mac ->
                        OutlinedButton(onClick = { vm.useReportedMac(onu, mac) }, enabled = !busy) { Text("Use the reported MAC") }
                    }
                } else {
                    Button(onClick = { vm.startLink(onu) }, enabled = !busy) { Text("Link to a house") }
                }
                if (!onu.inInventory) {
                    onu.macs.firstOrNull()?.let { mac ->
                        OutlinedButton(onClick = { vm.addToRegister(onu, mac) }, enabled = !busy) { Text("Add to the register") }
                    }
                }
            }
        }
    }
    if (confirmUnlink) {
        AlertDialog(
            onDismissRequest = { confirmUnlink = false },
            title = { Text("Unlink this ONU?") },
            text = { Text("The ONU is reset to factory settings when it next checks in, and it never gets this house's Wi-Fi again. The house keeps its saved Wi-Fi choice for its next ONU.") },
            confirmButton = { TextButton(onClick = { confirmUnlink = false; vm.unlink(onu) }) { Text("Unlink") } },
            dismissButton = { TextButton(onClick = { confirmUnlink = false }) { Text("Cancel") } },
        )
    }
}
