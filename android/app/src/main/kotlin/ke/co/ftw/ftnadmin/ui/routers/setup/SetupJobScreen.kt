package ke.co.ftw.ftnadmin.ui.routers.setup

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
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
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
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import ke.co.ftw.ftnadmin.core.network.UserInfo
import ke.co.ftw.ftnadmin.data.sites.RouterSetup
import ke.co.ftw.ftnadmin.data.sites.SetupState
import ke.co.ftw.ftnadmin.ui.common.AppTopBar
import ke.co.ftw.ftnadmin.ui.common.CenteredLoading
import ke.co.ftw.ftnadmin.ui.common.ChipRow
import ke.co.ftw.ftnadmin.ui.common.ErrorState
import ke.co.ftw.ftnadmin.ui.common.LabeledValue
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.common.LoadingSpinner
import ke.co.ftw.ftnadmin.ui.common.LocalContainer
import ke.co.ftw.ftnadmin.ui.common.SectionCard
import ke.co.ftw.ftnadmin.ui.common.StatusChip
import ke.co.ftw.ftnadmin.ui.common.Tone
import ke.co.ftw.ftnadmin.ui.common.vmFactory
import ke.co.ftw.ftnadmin.ui.theme.AppTheme
import kotlinx.coroutines.delay
import java.util.Locale

internal fun formatCountdown(totalSeconds: Long): String {
    val s = totalSeconds.coerceAtLeast(0)
    return String.format(Locale.US, "%d:%02d", s / 60, s % 60)
}

internal fun setupTone(state: String): Tone = when (state) {
    SetupState.LIVE -> Tone.Good
    SetupState.FAILED -> Tone.Bad
    SetupState.NEEDS_ACTION, SetupState.READY, SetupState.ROLLING_BACK -> Tone.Warn
    else -> Tone.Neutral
}

/** Automatic setup progress of one router: no Confirm button exists, only Start migration and the emergency Roll back now. */
@Composable
fun SetupJobScreen(routerId: String, user: UserInfo, onBack: () -> Unit, onJoin: () -> Unit) {
    val container = LocalContainer.current
    val vm: SetupJobViewModel = viewModel(
        key = "setup-job-$routerId",
        factory = vmFactory { SetupJobViewModel(routerId, container.siteSetup, container.sites) },
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
        topBar = { AppTopBar("Setup status", onBack = onBack) },
        snackbarHost = { SnackbarHost(snack) },
    ) { inner ->
        when (val c = ui.setup) {
            Loadable.Loading -> CenteredLoading(Modifier.padding(inner))
            is Loadable.Failed -> ErrorState(c.message, vm::retry, Modifier.padding(inner))
            is Loadable.Ready -> LazyColumn(
                Modifier.padding(inner).fillMaxSize(),
                contentPadding = PaddingValues(16.dp),
                verticalArrangement = Arrangement.spacedBy(12.dp),
            ) {
                val s = c.value
                item { ResultCard(s, ui) }
                if (s.state == SetupState.VERIFYING || s.state == SetupState.SWITCHING || s.state == SetupState.ARMING) {
                    if (s.deadlineAt != null || s.secondsLeft > 0) item { Countdown(s, ui.receivedAtMs) }
                    item { EvidenceCard(s) }
                }
                if (s.lastError.isNotBlank()) {
                    item {
                        SectionCard("What went wrong") {
                            Text(s.lastError, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodyMedium)
                        }
                    }
                }
                item { NextAction(s, ui, user, vm, onJoin) }
                if (s.blockers.isNotEmpty()) {
                    item {
                        SectionCard("Blocking checks") {
                            s.blockers.forEach { Text(it, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.error) }
                        }
                    }
                }
                if (s.steps.isNotEmpty()) item { SectionCard("Steps") { StepList(s.steps) } }
                if (user.canWrite && SetupState.canRollBack(s.state)) item { RollbackCard(ui, vm) }
            }
        }
    }

    if (ui.rollbackDialog) {
        AlertDialog(
            onDismissRequest = vm::closeRollback,
            title = { Text("Roll back now?") },
            text = { Text("The router returns to hotspot mode within about 30 seconds. Use this only if customers are offline and the automatic check has not fixed it.") },
            confirmButton = { TextButton(onClick = vm::rollbackNow) { Text("Roll back now") } },
            dismissButton = { TextButton(onClick = vm::closeRollback) { Text("Cancel") } },
        )
    }
}

@Composable
private fun ResultCard(s: RouterSetup, ui: SetupJobUiState) {
    val scheme = MaterialTheme.colorScheme
    val x = AppTheme.extended
    val (container, content) = when (s.state) {
        SetupState.LIVE -> x.successContainer to x.onSuccessContainer
        SetupState.FAILED -> scheme.errorContainer to scheme.onErrorContainer
        SetupState.ROLLED_BACK, SetupState.NEEDS_ACTION, SetupState.READY, SetupState.ROLLING_BACK -> x.warningContainer to x.onWarningContainer
        else -> scheme.secondaryContainer to scheme.onSecondaryContainer
    }
    Card(Modifier.fillMaxWidth(), colors = CardDefaults.cardColors(containerColor = container, contentColor = content)) {
        Column(Modifier.padding(20.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
            Text(ui.routerName.ifBlank { "Router" }, style = MaterialTheme.typography.labelLarge, fontWeight = FontWeight.Bold)
            Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                if (SetupState.isLive(s.state)) LoadingSpinner()
                Text(SetupState.label(s.state), style = MaterialTheme.typography.headlineMedium, fontWeight = FontWeight.ExtraBold, modifier = Modifier.weight(1f))
            }
            ChipRow(horizontalPadding = 0.dp) {
                when (s.kind) {
                    "fresh" -> StatusChip("New router", Tone.Neutral)
                    "migrate" -> StatusChip("Migration", Tone.Neutral)
                }
                if (ui.stale) StatusChip("Reconnecting", Tone.Warn)
                if (s.checks.ok + s.checks.warn + s.checks.fail > 0) {
                    StatusChip("${s.checks.ok} ok", Tone.Good)
                    if (s.checks.warn > 0) StatusChip("${s.checks.warn} warnings", Tone.Warn)
                    if (s.checks.fail > 0) StatusChip("${s.checks.fail} failed", Tone.Bad)
                }
            }
            val note = when (s.state) {
                SetupState.LIVE -> "Customers are served by the new setup. Nothing more to do."
                SetupState.ROLLED_BACK -> "The router went back to how it was. Paying customers keep working."
                SetupState.VERIFYING -> "The server confirms by itself when every flat is back online, or rolls back."
                SetupState.WAITING_ROUTER -> "Waiting for the router to join. Paste the join command on it."
                else -> ""
            }
            if (note.isNotBlank()) Text(note, style = MaterialTheme.typography.bodyMedium)
        }
    }
}

@Composable
private fun Countdown(s: RouterSetup, receivedAtMs: Long) {
    var now by remember { mutableLongStateOf(SystemClock.elapsedRealtime()) }
    LaunchedEffect(Unit) {
        while (true) {
            delay(500)
            now = SystemClock.elapsedRealtime()
        }
    }
    val left = s.secondsLeft - (now - receivedAtMs) / 1000
    SectionCard("Automatic check ends in") {
        Text(
            formatCountdown(left),
            style = MaterialTheme.typography.displayMedium,
            fontWeight = FontWeight.ExtraBold,
            color = if (left < 120) MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.onSurface,
            textAlign = TextAlign.Center,
            maxLines = 1,
            modifier = Modifier.fillMaxWidth(),
        )
        Text(
            "If the flats are not back by then, the router undoes the change by itself.",
            style = MaterialTheme.typography.bodySmall,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
    }
}

@Composable
private fun EvidenceCard(s: RouterSetup) {
    val e = s.evidence
    SectionCard("Evidence") {
        ChipRow(horizontalPadding = 0.dp) {
            StatusChip(if (e.tunnelUp) "Tunnel up" else "Tunnel down", if (e.tunnelUp) Tone.Good else Tone.Bad)
            StatusChip(
                "Flats bound ${e.flatsBound}/${e.flatsRequired}",
                if (e.flatsBound >= e.flatsRequired) Tone.Good else Tone.Warn,
            )
            StatusChip(if (e.internetOk) "Internet ok" else "No internet", if (e.internetOk) Tone.Good else Tone.Bad)
        }
    }
}

@Composable
private fun NextAction(s: RouterSetup, ui: SetupJobUiState, user: UserInfo, vm: SetupJobViewModel, onJoin: () -> Unit) {
    val a = s.nextAction
    if (a.kind == "none" && s.state != SetupState.READY && s.state != SetupState.FAILED) return
    SectionCard("What to do next") {
        if (a.text.isNotBlank()) Text(a.text, style = MaterialTheme.typography.bodyMedium)
        when {
            a.kind == "paste_join" -> if (user.isOwner) {
                Button(onClick = onJoin, modifier = Modifier.fillMaxWidth()) { Text("Get join command", maxLines = 1) }
            } else {
                Text("Only an owner can get the join command.", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            }
            a.kind == "migrate" || s.state == SetupState.READY -> if (user.canWrite) MigrateForm(s, ui, vm)
            a.kind == "retry" || s.state == SetupState.FAILED -> if (user.canWrite) {
                Button(onClick = vm::run, enabled = !ui.busy, modifier = Modifier.fillMaxWidth()) {
                    Text(if (ui.busy) "Working..." else "Try again", maxLines = 1)
                }
            }
            else -> Unit
        }
    }
}

@Composable
private fun MigrateForm(s: RouterSetup, ui: SetupJobUiState, vm: SetupJobViewModel) {
    Text(
        "This router already serves customers. Starting moves them to a fixed address per flat; the server checks every flat and rolls back by itself if any fail. Type the router name to start.",
        style = MaterialTheme.typography.bodyMedium,
    )
    OutlinedTextField(
        ui.typedName, vm::setTypedName, label = { Text(ui.routerName.ifBlank { "Router name" }) }, singleLine = true,
        modifier = Modifier.fillMaxWidth(),
    )
    Row(Modifier.fillMaxWidth(), verticalAlignment = Alignment.CenterVertically) {
        Column(Modifier.weight(1f).padding(end = 12.dp)) {
            Text("Keep unknown devices", style = MaterialTheme.typography.bodyLarge, fontWeight = FontWeight.Bold)
            Text(
                "Devices on the router that have no account stay connected after the switch.",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
        Switch(checked = ui.keepUnknown, onCheckedChange = vm::setKeepUnknown)
    }
    if (s.liveDevices > 0) LabeledValue("Live devices found", s.liveDevices.toString())
    Button(onClick = vm::startMigration, enabled = vm.canStart(), modifier = Modifier.fillMaxWidth()) {
        Text(if (ui.busy) "Starting..." else "Start migration", maxLines = 1)
    }
}

@Composable
private fun RollbackCard(ui: SetupJobUiState, vm: SetupJobViewModel) {
    SectionCard("Emergency") {
        Text(
            "Customers offline and the automatic check is not helping? Put the router back to hotspot mode now.",
            style = MaterialTheme.typography.bodySmall,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
        OutlinedButton(
            onClick = vm::openRollback,
            enabled = !ui.busy,
            colors = ButtonDefaults.outlinedButtonColors(contentColor = MaterialTheme.colorScheme.error),
            modifier = Modifier.fillMaxWidth(),
        ) { Text("Roll back now", maxLines = 1) }
    }
}
