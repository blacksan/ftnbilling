package ke.co.ftw.ftnadmin.ui.home

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.grid.GridCells
import androidx.compose.foundation.lazy.grid.GridItemSpan
import androidx.compose.foundation.lazy.grid.LazyGridScope
import androidx.compose.foundation.lazy.grid.LazyVerticalGrid
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.AccountBox
import androidx.compose.material.icons.filled.Build
import androidx.compose.material.icons.filled.DateRange
import androidx.compose.material.icons.filled.Home
import androidx.compose.material.icons.filled.Info
import androidx.compose.material.icons.filled.Notifications
import androidx.compose.material.icons.filled.Person
import androidx.compose.material.icons.filled.Place
import androidx.compose.material.icons.filled.Refresh
import androidx.compose.material.icons.filled.Settings
import androidx.compose.material.icons.filled.Share
import androidx.compose.material.icons.filled.Warning
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.pulltorefresh.PullToRefreshBox
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalDensity
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.LifecycleResumeEffect
import androidx.lifecycle.compose.LifecycleResumeEffect
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import ke.co.ftw.ftnadmin.core.network.UserInfo
import ke.co.ftw.ftnadmin.data.routers.Overview
import ke.co.ftw.ftnadmin.data.routers.RouterLive
import ke.co.ftw.ftnadmin.data.sites.SetupState
import ke.co.ftw.ftnadmin.domain.Site
import ke.co.ftw.ftnadmin.ui.common.CardTone
import ke.co.ftw.ftnadmin.ui.common.CenteredLoading
import ke.co.ftw.ftnadmin.ui.common.ChipRow
import ke.co.ftw.ftnadmin.ui.common.ErrorState
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.common.LocalContainer
import ke.co.ftw.ftnadmin.ui.common.StatCard
import ke.co.ftw.ftnadmin.ui.common.StatusChip
import ke.co.ftw.ftnadmin.ui.common.StatusDot
import ke.co.ftw.ftnadmin.ui.common.TabScaffold
import ke.co.ftw.ftnadmin.ui.common.Tone
import ke.co.ftw.ftnadmin.ui.common.formatAge
import ke.co.ftw.ftnadmin.ui.common.formatKes
import ke.co.ftw.ftnadmin.ui.common.formatPct
import ke.co.ftw.ftnadmin.ui.common.formatUptime
import ke.co.ftw.ftnadmin.ui.common.splitBps
import ke.co.ftw.ftnadmin.ui.common.vmFactory
import ke.co.ftw.ftnadmin.ui.update.UpdateCard
import java.util.Locale

private const val LARGE_FONT_SCALE = 1.3f
private val CARD_MIN_HEIGHT = 152.dp

/** Pill text and tone for the fleet: green only when every router is online. */
internal fun networkStatus(online: Int, total: Int): Pair<String, Tone> = when {
    total == 0 -> "No routers yet" to Tone.Neutral
    online >= total -> "All $total routers online" to Tone.Good
    online == 0 -> "0 of $total routers online" to Tone.Bad
    else -> "$online of $total routers online" to Tone.Bad
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun HomeScreen(
    user: UserInfo,
    onAlerts: () -> Unit,
    onSetup: (String) -> Unit,
    onRouter: (String) -> Unit,
    onExpiring: () -> Unit,
    onNewDevices: () -> Unit,
    onUnmatchedPayments: () -> Unit,
) {
    val container = LocalContainer.current
    val vm: HomeViewModel = viewModel(factory = vmFactory { HomeViewModel(container.routerLive, container.routerContext.selection) })
    val ui by vm.state.collectAsStateWithLifecycle()
    val update by container.updates.state.collectAsStateWithLifecycle()
    LifecycleResumeEffect(vm) {
        vm.startPolling()
        onPauseOrDispose { vm.stopPolling() }
    }
    // One column when text is scaled up, so numbers and labels are never squeezed.
    val columns = if (LocalDensity.current.fontScale >= LARGE_FONT_SCALE) 1 else 2

    TabScaffold(title = "Home", onAlerts = onAlerts) { inner ->
        val id = ui.routerId
        if (id != null) {
            when (val l = ui.live) {
                Loadable.Loading -> CenteredLoading(Modifier.padding(inner))
                is Loadable.Failed -> ErrorState(l.message, vm::retry, Modifier.padding(inner))
                is Loadable.Ready -> PullToRefreshBox(ui.refreshing, vm::refresh, Modifier.padding(inner).fillMaxSize()) {
                    CardGrid(columns) {
                        if (update.available != null) full { UpdateCard(update, container.updates) }
                        routerCards(l.value, ui, onAlerts, onExpiring, onSetup = { onSetup(id) })
                    }
                }
            }
        } else {
            when (val o = ui.overview) {
                Loadable.Loading -> CenteredLoading(Modifier.padding(inner))
                is Loadable.Failed -> ErrorState(o.message, vm::retry, Modifier.padding(inner))
                is Loadable.Ready -> PullToRefreshBox(ui.refreshing, vm::refresh, Modifier.padding(inner).fillMaxSize()) {
                    val routers by container.routerContext.routers.collectAsStateWithLifecycle()
                    CardGrid(columns) {
                        if (update.available != null) full { UpdateCard(update, container.updates) }
                        fleetCards(
                            o.value,
                            ui.staleError,
                            (routers as? Loadable.Ready)?.value.orEmpty(),
                            onPick = { container.routerContext.select(it) },
                            onOpen = onRouter,
                            onNewDevices = onNewDevices,
                            onUnmatched = onUnmatchedPayments,
                            onAlerts = onAlerts,
                        )
                    }
                }
            }
        }
    }
}

@Composable
private fun CardGrid(columns: Int, content: LazyGridScope.() -> Unit) {
    LazyVerticalGrid(
        columns = GridCells.Fixed(columns),
        modifier = Modifier.fillMaxSize(),
        contentPadding = PaddingValues(start = 16.dp, end = 16.dp, top = 4.dp, bottom = 24.dp),
        horizontalArrangement = Arrangement.spacedBy(12.dp),
        verticalArrangement = Arrangement.spacedBy(12.dp),
        content = content,
    )
}

private fun LazyGridScope.full(content: @Composable () -> Unit) {
    item(span = { GridItemSpan(maxLineSpan) }) { content() }
}

private fun LazyGridScope.card(content: @Composable () -> Unit) {
    item { content() }
}

private val Cardmod = Modifier.fillMaxWidth().heightIn(min = CARD_MIN_HEIGHT)

private fun LazyGridScope.routerCards(
    l: RouterLive,
    ui: HomeUiState,
    onAlerts: () -> Unit,
    onExpiring: () -> Unit,
    onSetup: () -> Unit,
) {
    full {
        Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
            ChipRow(horizontalPadding = 0.dp) {
                StatusChip(if (l.reachable) "Router online" else "Router unreachable", if (l.reachable) Tone.Good else Tone.Bad)
                StatusChip(if (l.provision.inSync) "In sync" else "${l.provision.drift} drifted", if (l.provision.inSync) Tone.Good else Tone.Warn)
                if (l.provision.pendingOps > 0) StatusChip("${l.provision.pendingOps} queued", Tone.Neutral)
            }
            if (!l.reachable) {
                Text(
                    "Showing the last numbers the server had. They update when the router is back.",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.error,
                )
            }
            ui.staleError?.let {
                Text("Could not refresh: $it", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.error)
            }
            if (l.provision.lastError.isNotBlank()) {
                Text(l.provision.lastError, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.error)
            }
        }
    }
    if (l.setupState.isNotBlank() && l.setupState != SetupState.READY) {
        full {
            StatCard(
                label = "Setup",
                value = SetupState.label(l.setupState),
                sub = "Tap to see the setup steps.",
                tone = CardTone.Info,
                icon = Icons.Default.Build,
                onClick = onSetup,
                modifier = Modifier.fillMaxWidth(),
            )
        }
    }
    val down = splitBps(l.wan.downBps)
    card {
        StatCard(
            "Online flats", l.flats.online.toString(), Cardmod,
            unit = "of ${l.flats.total}",
            sub = "${l.flats.active} active, ${l.flats.expired} expired",
            tone = CardTone.Feature, icon = Icons.Default.Person,
            spark = ui.onlineHistory,
        )
    }
    card {
        StatCard(
            "Throughput", down.first, Cardmod,
            unit = down.second,
            sub = "Up ${splitBps(l.wan.upBps).let { "${it.first} ${it.second}" }}",
            tone = CardTone.Brand, icon = Icons.Default.Share,
            spark = l.throughput.downBps.map { it.toDouble() },
        )
    }
    card {
        StatCard(
            "CPU", formatPct(l.system.cpuLoadPct), Cardmod,
            unit = "%",
            sub = if (l.system.cpuCount > 0) "${l.system.cpuCount} cores" else "",
            tone = if (l.system.cpuLoadPct >= 85) CardTone.Bad else CardTone.Info, icon = Icons.Default.Settings,
            spark = ui.cpuHistory,
        )
    }
    card {
        StatCard(
            "Memory", formatPct(l.system.memPct), Cardmod,
            unit = "%",
            sub = if (l.system.memTotalBytes > 0) "${l.system.memUsedBytes / MIB} of ${l.system.memTotalBytes / MIB} MiB" else "",
            tone = if (l.system.memPct >= 90) CardTone.Bad else CardTone.Teal, icon = Icons.Default.Info,
            spark = ui.memHistory,
        )
    }
    card {
        StatCard(
            "Uptime", formatUptime(l.system.uptimeS), Cardmod,
            sub = l.system.tempC?.let { String.format(Locale.US, "%.0f C", it) }.orEmpty(),
            tone = CardTone.Neutral, icon = Icons.Default.DateRange,
        )
    }
    card {
        StatCard(
            "RouterOS", l.system.version.ifBlank { "-" }, Cardmod,
            sub = listOf(l.system.board, l.system.model).filter { it.isNotBlank() }.distinct().joinToString(" "),
            tone = CardTone.Neutral, icon = Icons.Default.Refresh,
        )
    }
    card {
        StatCard(
            "WAN", if (l.wan.running) "Up" else "Down", Cardmod,
            sub = buildString {
                if (l.wan.iface.isNotBlank()) append(l.wan.iface).append(", ")
                append(formatPct(l.wan.utilPct)).append("% used")
                if (l.wan.capMbps > 0) append(" of ").append(l.wan.capMbps).append(" Mbps")
            },
            tone = if (l.wan.running) CardTone.Good else CardTone.Bad, icon = Icons.Default.Place,
        )
    }
    card {
        StatCard(
            "Tunnel", l.tunnel.rttMs?.let { formatPct(it) } ?: "-", Cardmod,
            unit = if (l.tunnel.rttMs != null) "ms" else "",
            sub = listOfNotNull(
                l.tunnel.handshakeAgeS?.let { "handshake $it s ago" },
                l.tunnel.lossPct?.let { "loss ${formatPct(it)}%" },
            ).joinToString(", "),
            tone = CardTone.Info, icon = Icons.Default.Share,
        )
    }
    card {
        StatCard(
            "ONTs", l.equipment.ont.toString(), Cardmod,
            unit = "on ${l.equipment.olt} OLT",
            sub = "${l.ontsBound} bound",
            tone = CardTone.Teal, icon = Icons.Default.Home,
        )
    }
    card {
        StatCard(
            "Revenue today", formatKes(l.revenue.todayKes).removePrefix("KES "), Cardmod,
            unit = "KES",
            sub = "Month ${formatKes(l.revenue.monthKes)}",
            tone = CardTone.Good, icon = Icons.Default.AccountBox,
        )
    }
    card {
        StatCard(
            "Expiring soon", l.flats.expiring7d.toString(), Cardmod,
            unit = "in 7 days",
            sub = "${l.flats.suspended} suspended",
            tone = if (l.flats.expiring7d > 0) CardTone.Warn else CardTone.Neutral, icon = Icons.Default.Warning,
            onClick = onExpiring,
        )
    }
    card {
        StatCard(
            "Alerts", l.openAlerts.toString(), Cardmod,
            unit = "open",
            sub = if (l.unregisteredOnline > 0) "${l.unregisteredOnline} new devices online" else "",
            tone = if (l.openAlerts > 0) CardTone.Bad else CardTone.Neutral, icon = Icons.Default.Notifications,
            onClick = onAlerts,
        )
    }
    full {
        Text(
            "Updated ${formatAge(l.generatedAt)}",
            style = MaterialTheme.typography.labelSmall,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
    }
}

private const val MIB = 1024L * 1024L

private fun LazyGridScope.fleetCards(
    o: Overview,
    staleError: String?,
    routers: List<Site>,
    onPick: (String) -> Unit,
    onOpen: (String) -> Unit,
    onNewDevices: () -> Unit,
    onUnmatched: () -> Unit,
    onAlerts: () -> Unit,
) {
    val (statusText, statusTone) = networkStatus(o.routers.online, o.routers.total)
    full {
        Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
            ChipRow(horizontalPadding = 0.dp) { StatusChip(statusText, statusTone) }
            staleError?.let { Text("Could not refresh: $it", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.error) }
        }
    }
    card {
        StatCard(
            "Revenue today", formatKes(o.revenue.todayKes).removePrefix("KES "), Cardmod,
            unit = "KES",
            sub = "Yesterday ${formatKes(o.revenue.yesterdayKes)}",
            tone = CardTone.Feature, icon = Icons.Default.AccountBox,
            spark = o.revenue.spark7d.map { it.toDouble() },
        )
    }
    card {
        StatCard("Month revenue", formatKes(o.revenue.monthKes).removePrefix("KES "), Cardmod, unit = "KES", tone = CardTone.Good, icon = Icons.Default.AccountBox)
    }
    card { StatCard("Active", o.active.toString(), Cardmod, unit = "flats", tone = CardTone.Brand, icon = Icons.Default.Person) }
    card { StatCard("Online now", o.onlineNow.toString(), Cardmod, unit = "devices", tone = CardTone.Teal, icon = Icons.Default.Share) }
    card {
        StatCard(
            "Routers online", "${o.routers.online}", Cardmod,
            unit = "of ${o.routers.total}",
            tone = if (o.routers.total > 0 && o.routers.online >= o.routers.total) CardTone.Good else CardTone.Bad,
            icon = Icons.Default.Home,
        )
    }
    card {
        StatCard(
            "Expiring soon", o.health.expiring7d.toString(), Cardmod,
            unit = "in 7 days",
            sub = "${o.health.atRisk} at risk, ${o.health.churned} churned",
            tone = if (o.health.expiring7d > 0) CardTone.Warn else CardTone.Neutral, icon = Icons.Default.Warning,
        )
    }
    if (o.next.isNotEmpty()) {
        full { SectionTitle("Needs attention") }
        o.next.forEach { n ->
            val action: (() -> Unit)? = when {
                n.kind.contains("device") -> onNewDevices
                n.kind.contains("unmatched") || n.kind.contains("payment") -> onUnmatched
                n.kind.contains("alert") -> onAlerts
                else -> null
            }
            full {
                StatCard(
                    n.text.ifBlank { n.kind }, n.count.toString(), Modifier.fillMaxWidth(),
                    tone = if (n.count > 0) CardTone.Warn else CardTone.Neutral,
                    icon = Icons.Default.Warning, onClick = action,
                )
            }
        }
    }
    if (routers.isNotEmpty()) {
        full { SectionTitle("Routers") }
        routers.forEach { r -> full { RouterRow(r, onClick = { onPick(r.key) }, onOpen = { onOpen(r.key) }) } }
    }
    if (o.busiestRouters.isNotEmpty()) {
        full { SectionTitle("Busiest right now") }
        o.busiestRouters.forEach { b ->
            val down = splitBps(b.downBps)
            full {
                StatCard(
                    b.name.ifBlank { b.routerId }, down.first, Modifier.fillMaxWidth(),
                    unit = down.second,
                    sub = "${formatPct(b.utilPct)}% of uplink, ${b.online} flats online",
                    tone = CardTone.Info, icon = Icons.Default.Share,
                    onClick = { onPick(b.routerId) },
                )
            }
        }
    }
}

@Composable
private fun SectionTitle(text: String) {
    Text(
        text,
        style = MaterialTheme.typography.titleLarge,
        fontWeight = FontWeight.ExtraBold,
        color = MaterialTheme.colorScheme.onBackground,
        modifier = Modifier.padding(top = 8.dp),
    )
}

@Composable
private fun RouterRow(r: Site, onClick: () -> Unit, onOpen: () -> Unit) {
    Card(
        onClick = onClick,
        modifier = Modifier.fillMaxWidth(),
        colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceContainerHigh),
    ) {
        Row(Modifier.padding(14.dp).fillMaxWidth(), verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(12.dp)) {
            StatusDot(r.online, size = 12.dp)
            Column(Modifier.weight(1f)) {
                Text(r.label, style = MaterialTheme.typography.titleMedium, fontWeight = FontWeight.Bold, maxLines = 1, overflow = TextOverflow.Ellipsis)
                Text(
                    "${r.accounts.active} active, ${r.accounts.total} flats" + if (r.openAlerts > 0) ", ${r.openAlerts} alerts" else "",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    maxLines = 2,
                )
            }
            TextButton(onClick = onOpen) { Text("Details", maxLines = 1, softWrap = false) }
        }
    }
}
