package ke.co.ftw.ftnadmin.ui.home

import ke.co.ftw.ftnadmin.ui.theme.AppTheme
import ke.co.ftw.ftnadmin.ui.common.LoadingSpinner
import android.net.Uri
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Refresh
import androidx.compose.material.icons.filled.Settings
import androidx.compose.material3.AssistChip
import androidx.compose.material3.Button
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableLongStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.compose.LifecycleEventEffect
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import androidx.lifecycle.viewmodel.initializer
import androidx.lifecycle.viewmodel.viewModelFactory
import ke.co.ftw.ftnadmin.net.Fetch
import ke.co.ftw.ftnadmin.net.FleetSummary
import ke.co.ftw.ftnadmin.net.ServerHealth
import ke.co.ftw.ftnadmin.net.SiteSummary
import ke.co.ftw.ftnadmin.tunnel.TunnelState
import ke.co.ftw.ftnadmin.tunnel.TunnelSummary
import ke.co.ftw.ftnadmin.ui.common.LocalContainer
import ke.co.ftw.ftnadmin.ui.common.SectionCard
import ke.co.ftw.ftnadmin.ui.common.formatAge
import ke.co.ftw.ftnadmin.ui.common.formatBytes
import ke.co.ftw.ftnadmin.ui.common.formatKes
import ke.co.ftw.ftnadmin.ui.common.rememberConnectAction
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun HomeScreen(onOpenAdmin: (path: String?) -> Unit, onOpenSettings: () -> Unit) {
    val container = LocalContainer.current
    val vm: HomeViewModel = viewModel(factory = viewModelFactory { initializer { HomeViewModel(container) } })
    val tunnelState by container.tunnel.state.collectAsStateWithLifecycle()
    val summary by container.tunnelRepository.summary.collectAsStateWithLifecycle()
    val publicHealth by vm.publicHealth.collectAsStateWithLifecycle()
    val tunnelHealth by vm.tunnelHealth.collectAsStateWithLifecycle()
    val fleet by vm.fleet.collectAsStateWithLifecycle()
    val connect = rememberConnectAction()
    val scope = rememberCoroutineScope()

    LifecycleEventEffect(Lifecycle.Event.ON_RESUME) { vm.refresh() }

    Scaffold(
        topBar = {
            TopAppBar(
                title = { Text("FTN Admin") },
                actions = {
                    IconButton(onClick = vm::refresh) { Icon(Icons.Default.Refresh, contentDescription = "Refresh") }
                    IconButton(onClick = onOpenSettings) { Icon(Icons.Default.Settings, contentDescription = "Settings") }
                },
            )
        },
    ) { padding ->
        Column(
            Modifier.fillMaxSize().padding(padding).verticalScroll(rememberScrollState()).padding(16.dp),
            verticalArrangement = Arrangement.spacedBy(12.dp),
        ) {
            Button(onClick = { onOpenAdmin(null) }, modifier = Modifier.fillMaxWidth()) { Text("Open admin") }

            TunnelCard(
                state = tunnelState,
                summary = summary,
                onConnect = connect,
                onDisconnect = { scope.launch { container.tunnel.disconnect() } },
            )
            ServerCard(publicHealth, tunnelHealth.takeIf { tunnelState is TunnelState.Up })
            FleetCard(
                tunnelUp = tunnelState is TunnelState.Up,
                fleet = fleet,
                onOpenSite = { id -> onOpenAdmin("/admin/sites/view?id=${Uri.encode(id)}") },
                onOpenAlerts = { onOpenAdmin("/admin/alerts") },
                onOpenAdmin = { onOpenAdmin(null) },
            )
        }
    }
}

@Composable
private fun TunnelCard(state: TunnelState, summary: TunnelSummary?, onConnect: () -> Unit, onDisconnect: () -> Unit) {
    SectionCard("Tunnel") {
        val (label, detail) = when (state) {
            TunnelState.NoConfig -> "Not set up" to null
            TunnelState.NeedsPermission -> "VPN permission needed" to "Allow the VPN request to connect."
            TunnelState.Down -> "Disconnected" to null
            is TunnelState.Connecting -> "Connecting" to state.stage
            is TunnelState.Up -> "Connected" to null
            is TunnelState.Error -> "Failed" to state.message
        }
        Text(label, style = MaterialTheme.typography.titleLarge, color = stateColor(state))
        detail?.let { Text(it, style = MaterialTheme.typography.bodyMedium) }
        if (state is TunnelState.Connecting) LoadingSpinner()
        if (state is TunnelState.Up) {
            val now = rememberNow()
            Text("Last handshake: ${formatAge(state.stats.lastHandshakeEpochMs, now)}")
            Text("Received ${formatBytes(state.stats.rxBytes)}, sent ${formatBytes(state.stats.txBytes)}")
        }
        summary?.let { Text("Server ${it.endpoint}", style = MaterialTheme.typography.bodySmall) }
        when (state) {
            is TunnelState.Up, is TunnelState.Connecting -> OutlinedButton(onClick = onDisconnect) { Text("Disconnect") }
            TunnelState.NoConfig -> Unit
            else -> Button(onClick = onConnect) { Text("Connect") }
        }
    }
}

@Composable
private fun stateColor(state: TunnelState) = when (state) {
    is TunnelState.Up -> AppTheme.extended.success
    is TunnelState.Connecting -> AppTheme.extended.warning
    is TunnelState.Error -> MaterialTheme.colorScheme.error
    else -> MaterialTheme.colorScheme.onSurface
}

/** Wall clock that ticks every second, for relative times. */
@Composable
private fun rememberNow(): Long {
    var now by remember { mutableLongStateOf(System.currentTimeMillis()) }
    LaunchedEffect(Unit) {
        while (true) {
            delay(1_000)
            now = System.currentTimeMillis()
        }
    }
    return now
}

@Composable
private fun ServerCard(publicHealth: Fetch<ServerHealth>, tunnel: Fetch<ServerHealth>?) {
    SectionCard("Server") {
        HealthBlock("Public (isp.ftw.co.ke)", publicHealth)
        if (tunnel != null) {
            HorizontalDivider()
            HealthBlock("Through the tunnel", tunnel)
        }
    }
}

@Composable
private fun HealthBlock(title: String, health: Fetch<ServerHealth>) {
    Column(verticalArrangement = Arrangement.spacedBy(4.dp)) {
        Text(title, style = MaterialTheme.typography.labelLarge)
        when (health) {
            Fetch.Loading -> LoadingSpinner()
            is Fetch.Ok -> {
                val h = health.value
                StatusRow("Server", h.ok)
                StatusRow("Routers (${h.sitesOnline}/${h.sitesTotal} sites online)", h.routerOk)
                StatusRow("Database${if (h.db.isNotEmpty()) " (${h.db})" else ""}", h.dbOk)
                Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    AssistChip(onClick = {}, label = { Text(if (h.mpesaEnabled) "M-Pesa on" else "M-Pesa off") })
                    if (h.testMode) AssistChip(onClick = {}, label = { Text("Test mode") })
                }
            }
            is Fetch.Failed -> Text(health.message, color = MaterialTheme.colorScheme.error)
            Fetch.AuthNeeded, Fetch.ServerTooOld -> Text("Unavailable")
        }
    }
}

@Composable
private fun StatusRow(label: String, ok: Boolean) {
    Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.SpaceBetween) {
        Text(label)
        Text(
            if (ok) "OK" else "Problem",
            style = MaterialTheme.typography.labelLarge,
            color = if (ok) AppTheme.extended.success else MaterialTheme.colorScheme.error,
        )
    }
}

@Composable
private fun FleetCard(
    tunnelUp: Boolean,
    fleet: Fetch<FleetSummary>,
    onOpenSite: (String) -> Unit,
    onOpenAlerts: () -> Unit,
    onOpenAdmin: () -> Unit,
) {
    SectionCard("Buildings") {
        if (!tunnelUp) {
            Text("Connect the tunnel to see the fleet.")
            return@SectionCard
        }
        when (fleet) {
            Fetch.Loading -> LoadingSpinner()
            Fetch.ServerTooOld -> Text("The server is too old for this view. Update the server; the admin pages still work.")
            Fetch.AuthNeeded -> {
                Text("Sign in to the admin to see the fleet.")
                OutlinedButton(onClick = onOpenAdmin) { Text("Sign in") }
            }
            is Fetch.Failed -> Text(fleet.message, color = MaterialTheme.colorScheme.error)
            is Fetch.Ok -> FleetContent(fleet.value, onOpenSite, onOpenAlerts)
        }
    }
}

@Composable
private fun FleetContent(f: FleetSummary, onOpenSite: (String) -> Unit, onOpenAlerts: () -> Unit) {
    val t = f.totals
    Text("${t.up} up, ${t.down} down, ${t.other} other of ${t.sites}")
    Text("Accounts ${t.accountsActive}/${t.accountsTotal} active")
    Text("Revenue today ${formatKes(t.revenueTodayKes)}, month ${formatKes(t.revenueMonthKes)}")
    AssistChip(
        onClick = onOpenAlerts,
        label = { Text(if (t.openAlerts == 0) "No open alerts" else "${t.openAlerts} open alerts") },
    )
    f.sites.forEach { site ->
        HorizontalDivider()
        SiteRow(site) { onOpenSite(site.id) }
    }
}

@Composable
private fun SiteRow(site: SiteSummary, onClick: () -> Unit) {
    Column(Modifier.fillMaxWidth().clickable(onClick = onClick).padding(vertical = 4.dp)) {
        Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.SpaceBetween) {
            Text(site.name.ifEmpty { site.id }, style = MaterialTheme.typography.titleSmall)
            Text(
                site.state.replaceFirstChar { it.uppercase() },
                color = when (site.state) {
                    "up" -> MaterialTheme.colorScheme.primary
                    "down" -> MaterialTheme.colorScheme.error
                    else -> MaterialTheme.colorScheme.onSurfaceVariant
                },
            )
        }
        Text(
            "${site.active}/${site.total} active, today ${formatKes(site.todayKes)}" +
                if (site.alerts > 0) ", ${site.alerts} alerts" else "",
            style = MaterialTheme.typography.bodySmall,
        )
        if (site.state == "down" && site.cause.isNotEmpty()) Text(site.cause, style = MaterialTheme.typography.bodySmall)
    }
}
