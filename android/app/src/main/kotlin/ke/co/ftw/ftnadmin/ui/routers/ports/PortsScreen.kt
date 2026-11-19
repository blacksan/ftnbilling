package ke.co.ftw.ftnadmin.ui.routers.ports

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.Button
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.pulltorefresh.PullToRefreshBox
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalLifecycleOwner
import androidx.compose.ui.unit.dp
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.LifecycleEventObserver
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import ke.co.ftw.ftnadmin.data.routers.PortRole
import ke.co.ftw.ftnadmin.data.routers.PortRow
import ke.co.ftw.ftnadmin.ui.common.AppTopBar
import ke.co.ftw.ftnadmin.ui.common.CenteredLoading
import ke.co.ftw.ftnadmin.ui.common.EmptyState
import ke.co.ftw.ftnadmin.ui.common.ErrorState
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.common.LocalContainer
import ke.co.ftw.ftnadmin.ui.common.SectionCard
import ke.co.ftw.ftnadmin.ui.common.StatusChip
import ke.co.ftw.ftnadmin.ui.common.Tone
import ke.co.ftw.ftnadmin.ui.common.formatBps
import ke.co.ftw.ftnadmin.ui.common.vmFactory

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun PortsScreen(routerId: String, routerName: String, onBack: () -> Unit) {
    val container = LocalContainer.current
    val vm: PortsViewModel = viewModel(
        key = "ports-$routerId",
        factory = vmFactory { PortsViewModel(routerId, routerName, container.routerLive) },
    )
    val ui by vm.state.collectAsStateWithLifecycle()
    val snack = remember { SnackbarHostState() }
    LaunchedEffect(ui.message) {
        ui.message?.let {
            snack.showSnackbar(it)
            vm.clearMessage()
        }
    }
    val owner = LocalLifecycleOwner.current
    DisposableEffect(owner, vm) {
        val observer = LifecycleEventObserver { _, e ->
            when (e) {
                Lifecycle.Event.ON_START -> vm.resume()
                Lifecycle.Event.ON_STOP -> vm.pause()
                else -> Unit
            }
        }
        owner.lifecycle.addObserver(observer)
        onDispose {
            owner.lifecycle.removeObserver(observer)
            vm.pause()
        }
    }

    Scaffold(
        containerColor = MaterialTheme.colorScheme.background,
        topBar = { AppTopBar("Ports", onBack = onBack) },
        snackbarHost = { SnackbarHost(snack) },
    ) { inner ->
        when (val p = ui.ports) {
            Loadable.Loading -> CenteredLoading(Modifier.padding(inner))
            is Loadable.Failed -> ErrorState(p.message, vm::retry, Modifier.padding(inner))
            is Loadable.Ready -> PullToRefreshBox(
                isRefreshing = ui.refreshing,
                onRefresh = vm::refresh,
                modifier = Modifier.padding(inner).fillMaxSize(),
            ) {
                LazyColumn(
                    Modifier.fillMaxSize(),
                    contentPadding = PaddingValues(16.dp),
                    verticalArrangement = Arrangement.spacedBy(12.dp),
                ) {
                    if (ui.stale || !p.value.reachable) {
                        item {
                            Text(
                                "The router did not answer; showing the last known state.",
                                color = MaterialTheme.colorScheme.error,
                                style = MaterialTheme.typography.bodySmall,
                            )
                        }
                    }
                    ke.co.ftw.ftnadmin.domain.customerPortLine(p.value.customerPort)?.let { line ->
                        item { Text(line, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant) }
                    }
                    if (p.value.items.isEmpty()) item { EmptyState("No ports reported.") }
                    items(p.value.items, key = { it.name }) { row ->
                        PortCard(
                            row,
                            busy = row.name in ui.busy,
                            onToggle = { vm.requestDisable(row) },
                            onRole = { vm.requestRole(row, it) },
                        )
                    }
                }
            }
        }
    }

    ui.pending?.let { pending ->
        AlertDialog(
            onDismissRequest = vm::cancel,
            title = { Text(pending.confirm.title) },
            text = {
                Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                    Text(pending.confirm.warning)
                    if (pending.confirm.needsName) {
                        Text("Type the router name ($routerName) to confirm.", style = MaterialTheme.typography.bodySmall)
                        OutlinedTextField(
                            value = pending.typed,
                            onValueChange = vm::setTyped,
                            singleLine = true,
                            modifier = Modifier.fillMaxWidth(),
                        )
                    }
                }
            },
            confirmButton = { Button(onClick = vm::confirm, enabled = vm.canConfirm()) { Text("Apply") } },
            dismissButton = { TextButton(onClick = vm::cancel) { Text("Cancel") } },
        )
    }
}

@Composable
private fun PortCard(p: PortRow, busy: Boolean, onToggle: () -> Unit, onRole: (String) -> Unit) {
    SectionCard(p.name) {
        Column(verticalArrangement = Arrangement.spacedBy(6.dp)) {
            Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                StatusChip(p.roleLabel.ifBlank { PortRole.label(p.role) }, Tone.Neutral)
                when {
                    p.disabled -> StatusChip("Disabled", Tone.Warn)
                    p.running -> StatusChip("Link up", Tone.Good)
                    else -> StatusChip("No link", Tone.Neutral)
                }
            }
            val detail = listOfNotNull(
                p.link.ifBlank { null },
                p.mac.ifBlank { null },
                if (p.rxBps != null && p.txBps != null) "rx ${formatBps(p.rxBps)}  tx ${formatBps(p.txBps)}" else null,
                p.flats?.let { "$it flats" },
                p.comment.ifBlank { null },
            )
            if (detail.isNotEmpty()) {
                Text(detail.joinToString("  ·  "), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            }
            p.apply?.takeIf { it.state != "ok" }?.let {
                Text("${it.state}: ${it.message}", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.error)
            }
            if (p.locked) {
                Text("Locked: the server never changes the internet uplink.", style = MaterialTheme.typography.bodySmall)
            } else if (!p.canChange) {
                Text(
                    if (p.needsOwner) "Only the owner can change this port." else "Your account cannot change this port.",
                    style = MaterialTheme.typography.bodySmall,
                )
            } else {
                var menu by remember { mutableStateOf(false) }
                Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    if (p.canDisable) {
                        OutlinedButton(onClick = onToggle, enabled = !busy) { Text(if (p.disabled) "Enable" else "Disable") }
                    } else {
                        Text("Move the OLT role to switch it off.", style = MaterialTheme.typography.bodySmall)
                    }
                    Column {
                        OutlinedButton(onClick = { menu = true }, enabled = !busy) { Text("Role") }
                        DropdownMenu(expanded = menu, onDismissRequest = { menu = false }) {
                            PortsViewModel.roleChoices(p).forEach { r ->
                                DropdownMenuItem(
                                    text = { Text(PortRole.label(r)) },
                                    onClick = {
                                        menu = false
                                        onRole(r)
                                    },
                                )
                            }
                        }
                    }
                }
            }
        }
    }
}
