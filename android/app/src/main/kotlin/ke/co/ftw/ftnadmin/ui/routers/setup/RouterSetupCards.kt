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
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
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

private const val DEFAULT_WAN = "ether1"
private const val DEFAULT_ADMIN = "ether2"

private fun adminPorts(lanPorts: String): List<String> =
    lanPorts.split(',').map { it.trim() }.filter { it.isNotEmpty() }.ifEmpty { listOf(DEFAULT_ADMIN) }

/** What a router built by the script has on: only the uplink and the admin port (same words as the web page). */
fun portsOffNotice(wanInterface: String, lanPorts: String): String =
    "After setup, only ${wanInterface.trim().ifEmpty { DEFAULT_WAN }} (internet) and ${adminPorts(lanPorts).joinToString(" and ")} (admin) are on. " +
        "Turn on the OLT and customer ports in the Ports tab, or customers stay offline."

/** Where the operator must be plugged in when the line is pasted. */
fun pasteAdminHint(lanPorts: String): String =
    "Be plugged into the admin port (${adminPorts(lanPorts).joinToString(" or ")}) when you paste the line: " +
        "every other port is switched off while the router builds itself."

/** The ports-off warning and the plug-in hint, shown with every setup line. */
@Composable
fun PortsOffNotice(wanInterface: String, lanPorts: String) {
    Column(verticalArrangement = Arrangement.spacedBy(4.dp)) {
        Text(
            portsOffNotice(wanInterface, lanPorts),
            style = MaterialTheme.typography.bodyMedium,
            color = MaterialTheme.colorScheme.error,
        )
        Text(
            pasteAdminHint(lanPorts),
            style = MaterialTheme.typography.bodySmall,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
    }
}

/** The one-line bootstrap command: the only RouterOS command the operator copies. */
@Composable
fun BootstrapLineCard(
    ui: RouterSetupUiState,
    isOwner: Boolean,
    onGet: () -> Unit,
    onHide: () -> Unit,
    wanInterface: String = "",
    lanPorts: String = "",
) {
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
                PortsOffNotice(wanInterface, lanPorts)
                line.error?.let { Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall) }
                Button(onClick = onGet, enabled = !line.busy, modifier = Modifier.fillMaxWidth()) {
                    Text(if (line.busy) "Getting line..." else "Get bootstrap line", maxLines = 1)
                }
            }
            else -> {
                PortsOffNotice(wanInterface, lanPorts)
                BootstrapCommandBlock(cmd)
                TextButton(onClick = onHide) { Text("Hide line") }
            }
        }
    }
}

/**
 * The one-time line with its expiry and a Copy button. The no-certificate-check variant is secondary, collapsed under
 * "Certificate error?". Used by Add router, the router's Setup page and the Reset result.
 */
@Composable
fun BootstrapCommandBlock(cmd: JoinCommand) {
    var showNoCheck by rememberSaveable { mutableStateOf(false) }
    Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
        val expiry = expiryText(cmd.expiresAt)
        if (expiry.isNotBlank()) Text(expiry, style = MaterialTheme.typography.labelLarge, color = MaterialTheme.colorScheme.primary)
        CommandBox(cmd.command)
        CopyButton("Copy", cmd.command)
        if (cmd.commandNocheck.isNotBlank()) {
            TextButton(onClick = { showNoCheck = !showNoCheck }) {
                Text(if (showNoCheck) "Hide certificate options" else "Certificate error?", maxLines = 1)
            }
            if (showNoCheck) {
                Text(
                    "Use this only if the router reports a certificate or clock error.",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
                CommandBox(cmd.commandNocheck)
                CopyButton("Copy without certificate check", cmd.commandNocheck)
            }
        }
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
