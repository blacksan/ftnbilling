package ke.co.ftw.ftnadmin.ui.routers.ports

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Warning
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.Icon
import androidx.compose.material3.Surface
import androidx.compose.ui.Alignment
import androidx.compose.ui.res.pluralStringResource
import androidx.compose.ui.res.stringResource
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
import ke.co.ftw.ftnadmin.R
import ke.co.ftw.ftnadmin.data.routers.PortRole
import ke.co.ftw.ftnadmin.ui.common.MonoText
import ke.co.ftw.ftnadmin.ui.common.portApplyLabel
import ke.co.ftw.ftnadmin.ui.theme.AppTheme
import ke.co.ftw.ftnadmin.data.routers.PortRow
import ke.co.ftw.ftnadmin.ui.common.AppTopBar
import ke.co.ftw.ftnadmin.ui.common.CenteredLoading
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
        topBar = { AppTopBar(stringResource(R.string.ports_title, routerName), onBack = onBack) },
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
                                stringResource(R.string.router_no_answer),
                                color = MaterialTheme.colorScheme.error,
                                style = MaterialTheme.typography.bodyMedium,
                            )
                        }
                    }
                    ke.co.ftw.ftnadmin.domain.customerPortLine(p.value.customerPort)?.let { line ->
                        item { Text(line, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant) }
                    }
                    if (p.value.items.isEmpty()) item { Text(stringResource(R.string.no_ports), color = MaterialTheme.colorScheme.onSurfaceVariant) }
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
                Column(Modifier.verticalScroll(rememberScrollState()), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                    Text(pending.confirm.warning)
                    if (pending.confirm.needsName) {
                        Text(stringResource(R.string.type_router_name, routerName), style = MaterialTheme.typography.bodyMedium)
                        OutlinedTextField(
                            value = pending.typed,
                            onValueChange = vm::setTyped,
                            label = { Text(stringResource(R.string.router_name)) },
                            singleLine = true,
                            modifier = Modifier.fillMaxWidth(),
                        )
                    }
                }
            },
            confirmButton = { Button(onClick = vm::confirm, enabled = vm.canConfirm()) { Text(stringResource(R.string.apply)) } },
            dismissButton = { TextButton(onClick = vm::cancel) { Text(stringResource(R.string.action_cancel)) } },
        )
    }
}

@OptIn(ExperimentalLayoutApi::class)
@Composable
private fun PortCard(p: PortRow, busy: Boolean, onToggle: () -> Unit, onRole: (String) -> Unit) {
    SectionCard(p.name) {
        Column(verticalArrangement = Arrangement.spacedBy(6.dp)) {
            FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                StatusChip(p.roleLabel.ifBlank { PortRole.label(p.role) }, Tone.Neutral)
                when {
                    p.disabled -> StatusChip(stringResource(R.string.port_disabled), Tone.Warn)
                    p.running -> StatusChip(stringResource(R.string.port_link_up), Tone.Good)
                    else -> StatusChip(stringResource(R.string.port_no_link), Tone.Neutral)
                }
            }
            if (p.mac.isNotBlank()) MonoText(p.mac, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            val housesText = p.flats?.let { pluralStringResource(R.plurals.houses_count, it, it) }
            val detail = listOfNotNull(
                p.link.ifBlank { null },
                if (p.rxBps != null && p.txBps != null) "rx ${formatBps(p.rxBps)}  tx ${formatBps(p.txBps)}" else null,
                housesText,
                p.comment.ifBlank { null },
            )
            if (detail.isNotEmpty()) {
                Text(detail.joinToString("  \u00B7  "), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            }
            if (p.note.isNotBlank()) {
                Text(p.note, style = MaterialTheme.typography.bodySmall)
            }
            if (p.warning.isNotBlank()) {
                // A warning is amber with an icon, not the blue tertiary colour, and it says "Warning" in words.
                val ext = AppTheme.extended
                Surface(shape = MaterialTheme.shapes.small, color = ext.warningContainer, contentColor = ext.onWarningContainer) {
                    Row(Modifier.padding(8.dp), horizontalArrangement = Arrangement.spacedBy(8.dp), verticalAlignment = Alignment.Top) {
                        Icon(Icons.Default.Warning, contentDescription = stringResource(R.string.warning))
                        Text(p.warning, style = MaterialTheme.typography.bodySmall, modifier = Modifier.weight(1f))
                    }
                }
            }
            if (p.disabled && !p.locked && p.role != PortRole.LAN) {
                Text(stringResource(R.string.port_switched_off), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            }
            p.apply?.takeIf { it.state != "ok" }?.let {
                Text("${portApplyLabel(it.state)}: ${it.message}", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.error)
            }
            if (p.locked) {
                Text(stringResource(R.string.port_locked), style = MaterialTheme.typography.bodySmall)
            } else if (!p.canChange) {
                Text(
                    stringResource(if (p.needsOwner) R.string.port_owner_only else R.string.port_cannot_change),
                    style = MaterialTheme.typography.bodySmall,
                )
            } else {
                var menu by remember { mutableStateOf(false) }
                // The note has its own line; the buttons wrap under it, so the Role button is never pushed off-screen.
                if (!p.canDisable) {
                    Text(stringResource(R.string.port_olt_note), style = MaterialTheme.typography.bodySmall)
                }
                FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                    if (p.canDisable) {
                        OutlinedButton(
                            onClick = onToggle,
                            enabled = !busy,
                            colors = if (p.disabled) ButtonDefaults.outlinedButtonColors() else ButtonDefaults.outlinedButtonColors(contentColor = MaterialTheme.colorScheme.error),
                        ) { Text(stringResource(if (p.disabled) R.string.port_enable else R.string.port_disable)) }
                    }
                    Box {
                        OutlinedButton(onClick = { menu = true }, enabled = !busy) { Text(stringResource(R.string.port_role)) }
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
