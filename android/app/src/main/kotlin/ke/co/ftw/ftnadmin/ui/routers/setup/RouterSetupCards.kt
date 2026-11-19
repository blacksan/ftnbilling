package ke.co.ftw.ftnadmin.ui.routers.setup

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import ke.co.ftw.ftnadmin.data.sites.JoinCommand
import ke.co.ftw.ftnadmin.data.sites.SetupState
import ke.co.ftw.ftnadmin.ui.common.ChipRow
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.common.LoadingSpinner
import ke.co.ftw.ftnadmin.ui.common.SectionCard
import ke.co.ftw.ftnadmin.ui.common.StatusChip
import ke.co.ftw.ftnadmin.ui.common.Tone

/** The four setup steps (script fetched, tunnel up, server login, checks green) and what to do next. */
@Composable
fun SetupStepsCard(ui: RouterSetupUiState, onRetry: () -> Unit, onChecks: () -> Unit) {
    SectionCard("Setup") {
        when (val c = ui.setup) {
            Loadable.Loading -> Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.Center) { LoadingSpinner() }
            is Loadable.Failed -> {
                Text(c.message, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodyMedium)
                OutlinedButton(onClick = onRetry, modifier = Modifier.fillMaxWidth()) { Text("Try again") }
            }
            is Loadable.Ready -> {
                val s = c.value
                ChipRow(horizontalPadding = 0.dp) {
                    StatusChip(
                        SetupState.label(s.state),
                        when (s.state) {
                            SetupState.READY -> Tone.Good
                            SetupState.ONLINE -> Tone.Warn
                            else -> Tone.Neutral
                        },
                    )
                    if (ui.stale) StatusChip("Reconnecting", Tone.Warn)
                    if (s.checks.fail > 0) StatusChip("${s.checks.fail} failed", Tone.Bad)
                }
                if (s.steps.isNotEmpty()) StepList(s.steps)
                if (s.nextAction.text.isNotBlank() && s.nextAction.kind != "none") {
                    Text(s.nextAction.text, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurface)
                }
                if (s.lastError.isNotBlank()) {
                    Text(s.lastError, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall)
                }
                s.join.report?.takeIf { !it.ok && it.lines.isNotEmpty() }?.let { r ->
                    Text("The router reported a problem", style = MaterialTheme.typography.labelLarge, color = MaterialTheme.colorScheme.error)
                    CommandBox(r.lines.joinToString("\n"))
                }
                if (!s.allGreen) {
                    Text(
                        "Pull down to refresh. This page also checks every few seconds.",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
                if (s.nextAction.kind == "fix" || s.blockers.isNotEmpty() || s.allGreen) {
                    OutlinedButton(onClick = onChecks, modifier = Modifier.fillMaxWidth()) { Text("Open checks") }
                }
            }
        }
    }
}

/** The one-line bootstrap command: the only RouterOS command the operator copies. */
@Composable
fun BootstrapLineCard(ui: RouterSetupUiState, isOwner: Boolean, onGet: () -> Unit, onHide: () -> Unit) {
    SectionCard("Bootstrap line") {
        val line = ui.line
        val cmd = line.command
        when {
            !isOwner -> Text(
                "Only an owner can get the bootstrap line.",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            cmd == null -> {
                Text(
                    "Paste one line in the terminal of a blank router. It erases itself and the server builds the rest. " +
                        "Getting a new line cancels the previous one. It is shown here only, never saved on this phone.",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
                line.error?.let { Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall) }
                Button(onClick = onGet, enabled = !line.busy, modifier = Modifier.fillMaxWidth()) {
                    Text(if (line.busy) "Getting line..." else "Get bootstrap line", maxLines = 1)
                }
            }
            else -> {
                BootstrapCommandBlock(cmd)
                TextButton(onClick = onHide) { Text("Hide line") }
            }
        }
    }
}

/** The one-time line with its expiry and a Copy button. Used by Add router and the router's Setup page. */
@Composable
fun BootstrapCommandBlock(cmd: JoinCommand) {
    Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
        val expiry = expiryText(cmd.expiresAt)
        if (expiry.isNotBlank()) Text(expiry, style = MaterialTheme.typography.labelLarge, color = MaterialTheme.colorScheme.primary)
        CommandBox(cmd.command)
        CopyButton("Copy", cmd.command)
    }
}

/** Owner-only danger card: wipes the router's join state on the server; the router is erased when a new line is pasted. */
@Composable
fun ResetRouterCard(onReset: () -> Unit) {
    SectionCard("Reset router") {
        Text(
            "Forgets this router's keys and login on the server and gives a new bootstrap line. Customers and the tunnel address are kept. " +
                "The router itself is erased only when you paste the new line.",
            style = MaterialTheme.typography.bodySmall,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
        Button(
            onClick = onReset,
            modifier = Modifier.fillMaxWidth(),
            colors = ButtonDefaults.buttonColors(containerColor = MaterialTheme.colorScheme.error, contentColor = MaterialTheme.colorScheme.onError),
        ) { Text("Reset router", maxLines = 1) }
    }
}

@Composable
fun ResetRouterDialog(ui: RouterSetupUiState, canReset: Boolean, onTyped: (String) -> Unit, onConfirm: () -> Unit, onClose: () -> Unit) {
    AlertDialog(
        onDismissRequest = onClose,
        title = { Text("Reset ${ui.routerName.ifBlank { "router" }}?") },
        text = {
            Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                Text("The server forgets this router and the current setup line stops working. Type the router name to continue.")
                OutlinedTextField(
                    ui.typedName, onTyped, label = { Text(ui.routerName.ifBlank { "Router name" }) }, singleLine = true,
                    isError = ui.resetError != null,
                    supportingText = ui.resetError?.let { e -> { Text(e) } },
                    modifier = Modifier.fillMaxWidth(),
                )
            }
        },
        confirmButton = { TextButton(onClick = onConfirm, enabled = canReset) { Text(if (ui.resetBusy) "Resetting..." else "Reset", maxLines = 1) } },
        dismissButton = { TextButton(onClick = onClose) { Text("Cancel") } },
    )
}
