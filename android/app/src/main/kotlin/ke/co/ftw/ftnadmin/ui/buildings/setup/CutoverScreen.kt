package ke.co.ftw.ftnadmin.ui.buildings.setup

import android.os.SystemClock
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableLongStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import ke.co.ftw.ftnadmin.core.network.UserInfo
import ke.co.ftw.ftnadmin.data.sites.Cutover
import ke.co.ftw.ftnadmin.data.sites.CutoverState
import ke.co.ftw.ftnadmin.ui.common.AppTopBar
import ke.co.ftw.ftnadmin.ui.common.CenteredLoading
import ke.co.ftw.ftnadmin.ui.common.ChipRow
import ke.co.ftw.ftnadmin.ui.common.ErrorState
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.common.LocalContainer
import ke.co.ftw.ftnadmin.ui.common.SectionCard
import ke.co.ftw.ftnadmin.ui.common.StatusChip
import ke.co.ftw.ftnadmin.ui.common.Tone
import ke.co.ftw.ftnadmin.ui.common.vmFactory
import kotlinx.coroutines.delay
import java.util.Locale

internal fun formatCountdown(totalSeconds: Long): String {
    val s = totalSeconds.coerceAtLeast(0)
    return String.format(Locale.US, "%d:%02d", s / 60, s % 60)
}

@Composable
fun CutoverScreen(siteId: String, user: UserInfo, onBack: () -> Unit) {
    val container = LocalContainer.current
    val vm: CutoverViewModel = viewModel(
        key = "cutover-$siteId",
        factory = vmFactory { CutoverViewModel(siteId, container.siteSetup, container.sites) },
    )
    val ui by vm.state.collectAsStateWithLifecycle()
    val snack = remember { SnackbarHostState() }
    LaunchedEffect(ui.message) {
        ui.message?.let {
            snack.showSnackbar(it)
            vm.clearMessage()
        }
    }

    Scaffold(
        containerColor = MaterialTheme.colorScheme.background,
        topBar = { AppTopBar("Cutover", onBack = onBack) },
        snackbarHost = { SnackbarHost(snack) },
    ) { inner ->
        when (val c = ui.cutover) {
            Loadable.Loading -> CenteredLoading(Modifier.padding(inner))
            is Loadable.Failed -> ErrorState(c.message, vm::retry, Modifier.padding(inner))
            is Loadable.Ready -> LazyColumn(
                Modifier.padding(inner).fillMaxSize(),
                contentPadding = PaddingValues(16.dp),
                verticalArrangement = Arrangement.spacedBy(12.dp),
            ) {
                item { StateCard(c.value, ui) }
                if (c.value.state == CutoverState.AWAITING_CONFIRM || c.value.state == CutoverState.RUNNING) {
                    item { Countdown(c.value, ui.receivedAtMs) }
                    item { EvidenceCard(c.value) }
                }
                if (c.value.lastError.isNotBlank()) {
                    item {
                        SectionCard("Last error") {
                            Text(c.value.lastError, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodyMedium)
                        }
                    }
                }
                if (c.value.steps.isNotEmpty()) item { SectionCard("Steps") { StepList(c.value.steps) } }
                item { Actions(c.value, ui, user, vm) }
            }
        }
    }

    when (ui.dialog) {
        CutoverDialog.Rollback -> AlertDialog(
            onDismissRequest = vm::closeDialog,
            title = { Text("Roll back?") },
            text = { Text("The router returns to how it was before the cutover. Flats lose their fixed addresses until the next attempt.") },
            confirmButton = { TextButton(onClick = vm::rollback) { Text("Roll back") } },
            dismissButton = { TextButton(onClick = vm::closeDialog) { Text("Cancel") } },
        )
        CutoverDialog.ConfirmAnyway -> AlertDialog(
            onDismissRequest = vm::closeDialog,
            title = { Text("Confirm anyway?") },
            text = {
                Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                    Text("Some evidence is missing. Confirming keeps the new setup and cancels the automatic rollback. Type the building name to continue.")
                    OutlinedTextField(
                        ui.forceName, vm::setForceName, label = { Text(ui.siteName) }, singleLine = true,
                        modifier = Modifier.fillMaxWidth(),
                    )
                }
            },
            confirmButton = {
                TextButton(onClick = { vm.confirmAnyway(user) }, enabled = user.isOwner && nameMatches(ui.forceName, ui.siteName)) { Text("Confirm anyway") }
            },
            dismissButton = { TextButton(onClick = vm::closeDialog) { Text("Cancel") } },
        )
        CutoverDialog.None -> Unit
    }
}

@Composable
private fun StateCard(c: Cutover, ui: CutoverUiState) {
    SectionCard(ui.siteName.ifBlank { "Cutover" }) {
        val (label, tone) = when (c.state) {
            CutoverState.RUNNING -> "Running" to Tone.Warn
            CutoverState.AWAITING_CONFIRM -> "Waiting for Confirm" to Tone.Warn
            CutoverState.CONFIRMED -> "Confirmed" to Tone.Good
            CutoverState.ROLLING_BACK -> "Rolling back" to Tone.Warn
            CutoverState.ROLLED_BACK -> "Rolled back" to Tone.Neutral
            CutoverState.FAILED -> "Failed" to Tone.Bad
            else -> "Not started" to Tone.Neutral
        }
        ChipRow(horizontalPadding = 0.dp) {
            StatusChip(label, tone)
            if (c.method == "script") StatusChip("Script method", Tone.Neutral)
            if (ui.stale) StatusChip("Reconnecting", Tone.Warn)
        }
        if (c.state == CutoverState.IDLE && c.blockers.isNotEmpty()) {
            Text(
                "Blocked by: ${c.blockers.joinToString(", ")}. Fix these on the Router setup screen first.",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.error,
            )
        }
    }
}

@Composable
private fun Countdown(c: Cutover, receivedAtMs: Long) {
    var now by remember { mutableLongStateOf(SystemClock.elapsedRealtime()) }
    LaunchedEffect(Unit) {
        while (true) {
            delay(500)
            now = SystemClock.elapsedRealtime()
        }
    }
    val left = c.secondsLeft - (now - receivedAtMs) / 1000
    SectionCard("Automatic rollback in") {
        Text(
            formatCountdown(left),
            style = MaterialTheme.typography.displayMedium,
            color = if (left < 300) MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.onSurface,
            textAlign = TextAlign.Center,
            maxLines = 1,
            modifier = Modifier.fillMaxWidth(),
        )
        Text(
            "If nobody confirms before this reaches zero, the router undoes the cutover by itself.",
            style = MaterialTheme.typography.bodySmall,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
    }
}

@Composable
private fun EvidenceCard(c: Cutover) {
    val e = c.evidence
    SectionCard("Evidence") {
        ChipRow(horizontalPadding = 0.dp) {
            StatusChip(if (e.tunnelUp) "Tunnel up" else "Tunnel down", if (e.tunnelUp) Tone.Good else Tone.Bad)
            StatusChip(
                "Flats bound ${e.flatsBound}/${e.flatsEntitled}",
                if (e.flatsEntitled > 0 && e.flatsBound >= e.flatsEntitled) Tone.Good else Tone.Warn,
            )
            StatusChip(if (e.internetOk) "Internet ok" else "No internet", if (e.internetOk) Tone.Good else Tone.Bad)
        }
    }
}

@Composable
private fun Actions(c: Cutover, ui: CutoverUiState, user: UserInfo, vm: CutoverViewModel) {
    Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
        when (c.state) {
            CutoverState.AWAITING_CONFIRM -> {
                if (user.canWrite) {
                    Button(onClick = vm::confirm, enabled = c.canConfirm && !ui.busy, modifier = Modifier.fillMaxWidth()) { Text("Confirm") }
                    if (!c.canConfirm) {
                        Text(
                            "Confirm is off until the tunnel is up, every flat is bound and the router reaches the internet.",
                            style = MaterialTheme.typography.bodySmall,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                        OutlinedButton(
                            onClick = { vm.openDialog(CutoverDialog.ConfirmAnyway) },
                            enabled = user.isOwner && !ui.busy,
                            modifier = Modifier.fillMaxWidth(),
                        ) { Text("Confirm anyway") }
                        if (!user.isOwner) {
                            Text("Only an owner can confirm anyway.", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                        }
                    }
                    RollbackButton(ui, vm)
                }
            }
            CutoverState.RUNNING -> if (user.canWrite) RollbackButton(ui, vm)
            CutoverState.IDLE, CutoverState.ROLLED_BACK, CutoverState.FAILED -> if (user.canWrite) StartForm(ui, vm)
            else -> Unit
        }
    }
}

@Composable
private fun RollbackButton(ui: CutoverUiState, vm: CutoverViewModel) {
    OutlinedButton(
        onClick = { vm.openDialog(CutoverDialog.Rollback) },
        enabled = !ui.busy,
        colors = ButtonDefaults.outlinedButtonColors(contentColor = MaterialTheme.colorScheme.error),
        modifier = Modifier.fillMaxWidth(),
    ) { Text("Roll back") }
}

@Composable
private fun StartForm(ui: CutoverUiState, vm: CutoverViewModel) {
    SectionCard("Start cutover") {
        Text(
            "Moves this building to IPoE with a fixed address per flat. Type the building name to confirm.",
            style = MaterialTheme.typography.bodyMedium,
            color = MaterialTheme.colorScheme.onSurface,
        )
        OutlinedTextField(
            ui.typedName, vm::setTypedName, label = { Text(ui.siteName.ifBlank { "Building name" }) }, singleLine = true,
            modifier = Modifier.fillMaxWidth(),
        )
        Row(Modifier.fillMaxWidth(), verticalAlignment = Alignment.CenterVertically) {
            Column(Modifier.weight(1f).padding(end = 12.dp)) {
                Text("Keep unknown devices", style = MaterialTheme.typography.bodyLarge, color = MaterialTheme.colorScheme.onSurface)
                Text(
                    "Devices on the router that have no account stay connected after the cutover.",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
            Switch(checked = ui.keepUnknown, onCheckedChange = vm::setKeepUnknown)
        }
        Button(onClick = vm::start, enabled = vm.canStart(), modifier = Modifier.fillMaxWidth()) {
            Text(if (ui.busy) "Starting..." else "Start cutover")
        }
    }
}
