package ke.co.ftw.ftnadmin.ui.more

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Build
import androidx.compose.material.icons.filled.Check
import androidx.compose.material.icons.filled.Home
import androidx.compose.material.icons.filled.Info
import androidx.compose.material.icons.filled.Lock
import androidx.compose.material.icons.filled.Notifications
import androidx.compose.material.icons.filled.Place
import androidx.compose.material.icons.filled.Refresh
import androidx.compose.material.icons.filled.Settings
import androidx.compose.material.icons.filled.Warning
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.FilterChip
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import ke.co.ftw.ftnadmin.core.network.UserInfo
import ke.co.ftw.ftnadmin.ui.common.ChipLabel
import ke.co.ftw.ftnadmin.ui.common.ChipRow
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.common.LocalContainer
import ke.co.ftw.ftnadmin.ui.common.StatusDot
import ke.co.ftw.ftnadmin.ui.common.TabScaffold
import ke.co.ftw.ftnadmin.ui.common.vmFactory
import ke.co.ftw.ftnadmin.ui.theme.AppTheme

@Composable
fun MoreScreen(
    user: UserInfo,
    onAlerts: () -> Unit,
    onSetupChecks: (String) -> Unit,
    onRouterSetup: (String) -> Unit,
    onEquipment: () -> Unit,
    onApartments: () -> Unit,
    onAlertList: () -> Unit,
    onSettings: () -> Unit,
) {
    val container = LocalContainer.current
    val ctx = container.routerContext
    val vm: RouterActionsViewModel = viewModel(
        factory = vmFactory { RouterActionsViewModel(container.siteSetup, ctx.selection, ctx.routers, user) },
    )
    val ui by vm.state.collectAsStateWithLifecycle()
    val routers by ctx.routers.collectAsStateWithLifecycle()
    val snack = remember { SnackbarHostState() }
    LaunchedEffect(ui.message) {
        ui.message?.let {
            snack.showSnackbar(it)
            vm.clearMessage()
        }
    }
    val id = ui.routerId
    val s = MaterialTheme.colorScheme
    val x = AppTheme.extended

    TabScaffold(title = "More", onAlerts = onAlerts, snackbarHost = { SnackbarHost(snack) }) { inner ->
        Column(
            Modifier.padding(inner).fillMaxSize().verticalScroll(rememberScrollState()).padding(horizontal = 16.dp, vertical = 8.dp),
            verticalArrangement = Arrangement.spacedBy(10.dp),
        ) {
            Heading(if (ui.routerName.isNotBlank()) "Router actions: ${ui.routerName}" else "Router actions")
            if (id == null) {
                Text(
                    "Pick a router to use these actions, here or in the top bar.",
                    style = MaterialTheme.typography.bodyMedium,
                    color = s.onSurfaceVariant,
                )
                ChipRow(horizontalPadding = 0.dp) {
                    (routers as? Loadable.Ready)?.value.orEmpty().forEach { r ->
                        FilterChip(
                            selected = false,
                            onClick = { ctx.select(r.key) },
                            leadingIcon = { StatusDot(r.online, size = 10.dp) },
                            label = { ChipLabel(r.label) },
                        )
                    }
                }
            } else {
                ActionCard("Setup and checks", "Router checks and fixes", Icons.Default.Build, s.primaryContainer, s.onPrimaryContainer, s.primary, s.onPrimary) { onSetupChecks(id) }
                ActionCard("Router setup", "Setup steps, bootstrap line and reset", Icons.Default.Info, s.secondaryContainer, s.onSecondaryContainer, s.secondary, s.onSecondary) { onRouterSetup(id) }
                if (user.canWrite) {
                    ActionCard(
                        "Recheck", "Read every check from the router again", Icons.Default.Check,
                        s.tertiaryContainer, s.onTertiaryContainer, s.tertiary, s.onTertiary,
                        busy = ui.busy == "recheck",
                    ) { vm.recheck() }
                    ActionCard(
                        "Resync", "Re-apply every customer to the router", Icons.Default.Refresh,
                        x.successContainer, x.onSuccessContainer, x.success, x.onSuccess,
                        busy = ui.busy == "resync",
                    ) { vm.resync() }
                }
                if (user.isOwner) {
                    ActionCard(
                        "Reboot", "Restarts the router. Type its name to confirm.", Icons.Default.Warning,
                        s.errorContainer, s.onErrorContainer, s.error, s.onError,
                        busy = ui.busy == "reboot",
                    ) { vm.openReboot() }
                } else {
                    Text("Reboot is for owners.", style = MaterialTheme.typography.bodySmall, color = s.onSurfaceVariant)
                }
            }

            Heading("Manage")
            ActionCard("Equipment", "Routers, OLTs, ONUs and other equipment", Icons.Default.Place, s.primaryContainer, s.onPrimaryContainer, s.primary, s.onPrimary, onClick = onEquipment)
            ActionCard("Apartments", "Apartments, their routers, houses, caretaker and landlord", Icons.Default.Home, s.tertiaryContainer, s.onTertiaryContainer, s.tertiary, s.onTertiary, onClick = onApartments)
            ActionCard("Alerts", "Open alerts and muted routers", Icons.Default.Notifications, x.warningContainer, x.onWarningContainer, x.warning, x.onWarning, onClick = onAlertList)
            ActionCard("App settings", "Theme, app lock, updates, devices and sign out", Icons.Default.Settings, s.secondaryContainer, s.onSecondaryContainer, s.secondary, s.onSecondary, onClick = onSettings)
        }
    }

    if (ui.rebootDialog) {
        AlertDialog(
            onDismissRequest = vm::closeReboot,
            title = { Text("Reboot ${ui.routerName}?") },
            text = {
                Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                    Text("Customers on this router lose internet for about a minute. Type the router name to continue.")
                    OutlinedTextField(
                        ui.typedName, vm::setTypedName, label = { Text(ui.routerName.ifBlank { "Router name" }) }, singleLine = true,
                        modifier = Modifier.fillMaxWidth(),
                    )
                }
            },
            confirmButton = { TextButton(onClick = vm::reboot, enabled = vm.canReboot()) { Text("Reboot", maxLines = 1) } },
            dismissButton = { TextButton(onClick = vm::closeReboot) { Text("Cancel") } },
        )
    }
}

@Composable
private fun Heading(text: String) {
    Text(
        text,
        style = MaterialTheme.typography.titleLarge,
        fontWeight = FontWeight.ExtraBold,
        color = MaterialTheme.colorScheme.onBackground,
        maxLines = 2,
        overflow = TextOverflow.Ellipsis,
        modifier = Modifier.padding(top = 6.dp),
    )
}

@Composable
private fun ActionCard(
    title: String,
    subtitle: String,
    icon: ImageVector,
    container: Color,
    content: Color,
    accent: Color,
    onAccent: Color,
    busy: Boolean = false,
    onClick: () -> Unit,
) {
    Card(
        onClick = onClick,
        enabled = !busy,
        modifier = Modifier.fillMaxWidth(),
        colors = CardDefaults.cardColors(containerColor = container, contentColor = content),
    ) {
        Row(Modifier.padding(14.dp).fillMaxWidth(), verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(14.dp)) {
            Surface(shape = CircleShape, color = accent, contentColor = onAccent, modifier = Modifier.size(40.dp)) {
                Icon(icon, contentDescription = null, modifier = Modifier.padding(9.dp))
            }
            Column(Modifier.weight(1f)) {
                Text(if (busy) "$title..." else title, style = MaterialTheme.typography.titleMedium, fontWeight = FontWeight.ExtraBold, maxLines = 1, overflow = TextOverflow.Ellipsis)
                Text(subtitle, style = MaterialTheme.typography.bodySmall, maxLines = 2, overflow = TextOverflow.Ellipsis)
            }
        }
    }
}
