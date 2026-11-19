package ke.co.ftw.ftnadmin.ui.routers.setup

import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FilterChip
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.pulltorefresh.PullToRefreshBox
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalClipboardManager
import androidx.compose.ui.text.AnnotatedString
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import ke.co.ftw.ftnadmin.core.network.UserInfo
import ke.co.ftw.ftnadmin.data.sites.CheckItem
import ke.co.ftw.ftnadmin.data.sites.CheckStatus
import ke.co.ftw.ftnadmin.data.sites.Checks
import ke.co.ftw.ftnadmin.data.sites.FixKind
import ke.co.ftw.ftnadmin.data.sites.JoinReport
import ke.co.ftw.ftnadmin.data.sites.Step
import ke.co.ftw.ftnadmin.ui.common.AppTopBar
import ke.co.ftw.ftnadmin.ui.common.CenteredLoading
import ke.co.ftw.ftnadmin.ui.common.ChipLabel
import ke.co.ftw.ftnadmin.ui.common.ChipRow
import ke.co.ftw.ftnadmin.ui.common.ErrorState
import ke.co.ftw.ftnadmin.ui.common.LabeledValue
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.common.LocalContainer
import ke.co.ftw.ftnadmin.ui.common.SectionCard
import ke.co.ftw.ftnadmin.ui.common.StatusChip
import ke.co.ftw.ftnadmin.ui.common.Tone
import ke.co.ftw.ftnadmin.ui.common.formatAge
import ke.co.ftw.ftnadmin.ui.common.formatDateTime
import ke.co.ftw.ftnadmin.ui.common.PackagePicker
import ke.co.ftw.ftnadmin.ui.common.parseInstant
import ke.co.ftw.ftnadmin.ui.common.vmFactory
import ke.co.ftw.ftnadmin.ui.theme.AppTheme
import java.time.Duration
import java.time.Instant

private val GROUPS = listOf("router" to "Router", "network" to "Network", "devices" to "Devices", "services" to "Services")

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun SetupScreen(siteId: String, user: UserInfo, onBack: () -> Unit) {
    val container = LocalContainer.current
    val vm: SetupViewModel = viewModel(
        key = "setup-$siteId",
        factory = vmFactory { SetupViewModel(siteId, container.siteSetup) },
    )
    val bench: BenchViewModel = viewModel(
        key = "bench-$siteId",
        factory = vmFactory { BenchViewModel(siteId, container.siteSetup, container.newDevices, container.accounts) },
    )
    val ui by vm.state.collectAsStateWithLifecycle()
    val benchUi by bench.state.collectAsStateWithLifecycle()
    val snack = remember { SnackbarHostState() }
    LaunchedEffect(ui.message) {
        ui.message?.let {
            snack.showSnackbar(it)
            vm.clearMessage()
        }
    }
    LaunchedEffect(benchUi.message) {
        benchUi.message?.let {
            snack.showSnackbar(it)
            bench.clearMessage()
        }
    }

    Scaffold(
        containerColor = MaterialTheme.colorScheme.background,
        topBar = { AppTopBar("Setup and checks", onBack = onBack) },
        snackbarHost = { SnackbarHost(snack) },
    ) { inner ->
        when (val c = ui.checks) {
            Loadable.Loading -> CenteredLoading(Modifier.padding(inner))
            is Loadable.Failed -> ErrorState(c.message, vm::retry, Modifier.padding(inner))
            is Loadable.Ready -> PullToRefreshBox(ui.refreshing, vm::refresh, Modifier.padding(inner).fillMaxSize()) {
                LazyColumn(
                    Modifier.fillMaxSize(),
                    contentPadding = PaddingValues(16.dp),
                    verticalArrangement = Arrangement.spacedBy(12.dp),
                ) {
                    item { RouterSummary(c.value) }
                    c.value.joinReport?.let { r -> item { JoinReportCard(r) } }
                    val known = GROUPS.map { it.first }.toSet()
                    val groups = GROUPS + c.value.items.map { it.group }.filter { it !in known }.distinct().map { it to it.replaceFirstChar { ch -> ch.uppercase() } }
                    groups.forEach { (key, title) ->
                        val items = c.value.items.filter { it.group == key }
                        if (items.isNotEmpty()) {
                            item(key = "h-$key") {
                                Text(title, style = MaterialTheme.typography.titleMedium, color = MaterialTheme.colorScheme.onBackground)
                            }
                            items(items.size, key = { "c-${items[it].id}" }) { i ->
                                CheckCard(items[i], user, busy = items[i].id in ui.fixing, onFix = { vm.requestFix(items[i]) })
                            }
                        }
                    }
                    if (user.canWrite) item { BenchCard(benchUi, bench) }
                }
            }
        }
    }

    ui.confirmFix?.let { item ->
        AlertDialog(
            onDismissRequest = vm::cancelFix,
            title = { Text(item.fix?.label.orEmpty().ifBlank { item.title }) },
            text = { Text(item.fix?.confirmText.orEmpty().ifBlank { "Apply this change on the router?" }) },
            confirmButton = { TextButton(onClick = vm::confirmFix) { Text("Apply") } },
            dismissButton = { TextButton(onClick = vm::cancelFix) { Text("Cancel") } },
        )
    }
}

@Composable
private fun statusColor(status: String) = when (status) {
    CheckStatus.OK -> AppTheme.extended.success
    CheckStatus.WARN -> AppTheme.extended.warning
    CheckStatus.FAIL -> MaterialTheme.colorScheme.error
    CheckStatus.INFO -> MaterialTheme.colorScheme.secondary
    else -> MaterialTheme.colorScheme.outline
}

private fun statusTone(status: String) = when (status) {
    CheckStatus.OK -> Tone.Good
    CheckStatus.WARN -> Tone.Warn
    CheckStatus.FAIL -> Tone.Bad
    else -> Tone.Neutral
}

internal fun statusLabel(status: String) = when (status) {
    CheckStatus.OK -> "OK"
    CheckStatus.WARN -> "Warning"
    CheckStatus.FAIL -> "Failed"
    CheckStatus.INFO -> "Info"
    else -> "Unknown"
}

@Composable
private fun RouterSummary(c: Checks) {
    SectionCard("Router") {
        ChipRow(horizontalPadding = 0.dp) {
            StatusChip(if (c.router.reachable) "Reachable" else "Unreachable", if (c.router.reachable) Tone.Good else Tone.Bad)
            if (c.router.version.isNotBlank()) StatusChip("RouterOS ${c.router.version}", Tone.Neutral)
        }
        ChipRow(horizontalPadding = 0.dp) {
            StatusChip("${c.summary.ok} ok", Tone.Good)
            StatusChip("${c.summary.warn} warnings", Tone.Warn)
            StatusChip("${c.summary.fail} failed", Tone.Bad)
            StatusChip("${c.summary.unknown} unknown", Tone.Neutral)
        }
        if (c.router.board.isNotBlank()) LabeledValue("Board", c.router.board)
        LabeledValue("Checked", formatAge(c.generatedAt))
        Text(
            "Pull down to read the router again.",
            style = MaterialTheme.typography.bodySmall,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
    }
}

@Composable
internal fun CommandBox(text: String, modifier: Modifier = Modifier) {
    Surface(modifier.fillMaxWidth(), shape = MaterialTheme.shapes.small, color = MaterialTheme.colorScheme.surfaceContainerHighest) {
        Text(
            text,
            style = MaterialTheme.typography.bodySmall.copy(fontFamily = FontFamily.Monospace),
            color = MaterialTheme.colorScheme.onSurface,
            modifier = Modifier.padding(10.dp),
        )
    }
}

@Composable
internal fun CopyButton(label: String, text: String, modifier: Modifier = Modifier) {
    val clipboard = LocalClipboardManager.current
    OutlinedButton(onClick = { clipboard.setText(AnnotatedString(text)) }, modifier = modifier.fillMaxWidth()) { Text(label) }
}

internal fun expiryText(expiresAt: String, now: Instant = Instant.now()): String {
    val t = parseInstant(expiresAt) ?: return ""
    val secs = Duration.between(now, t).seconds
    return if (secs <= 0) "Expired. Get a new command." else "Expires in ${(secs + 59) / 60} min (${formatDateTime(expiresAt)})"
}

@Composable
private fun JoinReportCard(r: JoinReport) {
    SectionCard("Last join report") {
        ChipRow(horizontalPadding = 0.dp) {
            StatusChip(if (r.ok) "Join succeeded" else "Join had problems", if (r.ok) Tone.Good else Tone.Bad)
            StatusChip(formatAge(r.at), Tone.Neutral)
        }
        if (r.lines.isNotEmpty()) CommandBox(r.lines.joinToString("\n"))
    }
}

@Composable
private fun CheckCard(item: CheckItem, user: UserInfo, busy: Boolean, onFix: () -> Unit) {
    Card(Modifier.fillMaxWidth(), colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceContainer)) {
        Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(6.dp)) {
            Row(verticalAlignment = Alignment.Top) {
                Surface(Modifier.padding(top = 6.dp).size(12.dp), shape = CircleShape, color = statusColor(item.status)) {}
                Text(
                    item.title.ifBlank { item.id },
                    style = MaterialTheme.typography.titleSmall,
                    color = MaterialTheme.colorScheme.onSurface,
                    modifier = Modifier.weight(1f).padding(start = 10.dp),
                )
            }
            ChipRow(horizontalPadding = 0.dp) {
                StatusChip(statusLabel(item.status), statusTone(item.status))
            }
            if (item.detail.isNotBlank()) {
                Text(item.detail, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurface)
            }
            item.evidence.forEach {
                Text(
                    it,
                    style = MaterialTheme.typography.bodySmall.copy(fontFamily = FontFamily.Monospace),
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
            FixArea(item, user, busy, onFix)
        }
    }
}

@Composable
private fun FixArea(item: CheckItem, user: UserInfo, busy: Boolean, onFix: () -> Unit) {
    val fix = item.fix ?: return
    if (item.status == CheckStatus.OK || fix.kind == FixKind.NONE) return
    when (fix.kind) {
        FixKind.AUTO, FixKind.CONFIRM -> {
            Button(onClick = onFix, enabled = canRunFix(user, fix) && !busy, modifier = Modifier.fillMaxWidth()) {
                Text(if (busy) "Working..." else fix.label.ifBlank { "Fix" })
            }
            if (!canRunFix(user, fix)) {
                Text(
                    if (fix.role == "owner") "Only an owner can run this fix." else "Your role cannot run fixes.",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
        }
        FixKind.MANUAL -> {
            if (fix.label.isNotBlank()) Text(fix.label, style = MaterialTheme.typography.labelLarge, color = MaterialTheme.colorScheme.onSurface)
        }
    }
    if (fix.physical) {
        Text(
            "Needs physical access to the router (a button press or a power cut).",
            style = MaterialTheme.typography.bodySmall,
            color = AppTheme.extended.warning,
        )
    }
}

@Composable
internal fun StepList(steps: List<Step>) {
    Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
        steps.forEach { s ->
            Row(verticalAlignment = Alignment.Top) {
                val color = when (s.status) {
                    "ok" -> AppTheme.extended.success
                    "failed" -> MaterialTheme.colorScheme.error
                    "running" -> MaterialTheme.colorScheme.primary
                    else -> MaterialTheme.colorScheme.outline
                }
                Surface(Modifier.padding(top = 6.dp).size(12.dp), shape = CircleShape, color = color) {}
                Column(Modifier.weight(1f).padding(start = 10.dp)) {
                    Text("${s.n}. ${s.title.ifBlank { s.key }}", style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurface)
                    Text(
                        s.status.replaceFirstChar { it.uppercase() } + if (s.detail.isNotBlank()) ": ${s.detail}" else "",
                        style = MaterialTheme.typography.bodySmall,
                        color = if (s.status == "failed") MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
            }
        }
    }
}

@Composable
private fun BenchCard(ui: BenchUiState, vm: BenchViewModel) {
    SectionCard("Bench tools") {
        Text(
            "Make a test flat from a device learned on the bench port, then check that suspend and unsuspend reach the router.",
            style = MaterialTheme.typography.bodySmall,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
        if (ui.devices.isNotEmpty()) {
            Text("Learned devices", style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
            ChipRow(horizontalPadding = 0.dp) {
                ui.devices.forEach { d ->
                    FilterChip(selected = ui.mac.equals(d.mac, ignoreCase = true), onClick = { vm.setMac(d.mac) }, label = { ChipLabel(d.mac) })
                }
            }
        }
        OutlinedTextField(
            ui.mac, vm::setMac, label = { Text("MAC address") }, singleLine = true,
            isError = vm.macError() != null, supportingText = vm.macError()?.let { e -> { Text(e) } },
            modifier = Modifier.fillMaxWidth(),
        )
        Text("Package", style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
        PackagePicker(ui.packages, ui.packageId, vm::setPackage)
        Button(onClick = vm::createTestFlat, enabled = vm.canCreate(), modifier = Modifier.fillMaxWidth()) {
            Text(if (ui.busy) "Working..." else "Create test flat")
        }
        ui.testAccount?.let { a ->
            LabeledValue("Test flat", a.flat.ifBlank { a.code })
            OutlinedButton(onClick = vm::runSuspendTest, enabled = !ui.busy, modifier = Modifier.fillMaxWidth()) { Text("Run suspend test") }
        }
        if (ui.suspendSteps.isNotEmpty()) StepList(ui.suspendSteps)
    }
}
