package ke.co.ftw.ftnadmin.ui.buildings

import androidx.compose.foundation.Canvas
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FilterChip
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.pulltorefresh.PullToRefreshBox
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.graphics.Path
import androidx.compose.ui.graphics.StrokeCap
import androidx.compose.ui.graphics.drawscope.Stroke
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import ke.co.ftw.ftnadmin.domain.Ont
import ke.co.ftw.ftnadmin.domain.Qoe
import ke.co.ftw.ftnadmin.domain.Site
import ke.co.ftw.ftnadmin.ui.common.AppTopBar
import ke.co.ftw.ftnadmin.ui.common.CenteredLoading
import ke.co.ftw.ftnadmin.ui.common.ChipLabel
import ke.co.ftw.ftnadmin.ui.common.ChipRow
import ke.co.ftw.ftnadmin.ui.common.EmptyState
import ke.co.ftw.ftnadmin.ui.common.ErrorState
import ke.co.ftw.ftnadmin.ui.common.LabeledValue
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.common.LoadingSpinner
import ke.co.ftw.ftnadmin.ui.common.LocalContainer
import ke.co.ftw.ftnadmin.ui.common.SectionCard
import ke.co.ftw.ftnadmin.ui.common.StatusChip
import ke.co.ftw.ftnadmin.ui.common.Tone
import ke.co.ftw.ftnadmin.ui.common.formatAge
import ke.co.ftw.ftnadmin.ui.common.formatBps
import ke.co.ftw.ftnadmin.ui.common.vmFactory
import java.util.Locale

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun BuildingsScreen(onSettings: () -> Unit, onOpen: (String) -> Unit) {
    val container = LocalContainer.current
    val vm: BuildingsViewModel = viewModel(factory = vmFactory { BuildingsViewModel(container.sites) })
    val ui by vm.state.collectAsStateWithLifecycle()
    Scaffold(containerColor = MaterialTheme.colorScheme.background, topBar = { AppTopBar("Buildings", onSettings = onSettings) }) { inner ->
        when (val s = ui.sites) {
            Loadable.Loading -> CenteredLoading(Modifier.padding(inner))
            is Loadable.Failed -> ErrorState(s.message, vm::retry, Modifier.padding(inner))
            is Loadable.Ready -> PullToRefreshBox(ui.refreshing, vm::refresh, Modifier.padding(inner).fillMaxSize()) {
                if (s.value.isEmpty()) {
                    EmptyState("No buildings yet.")
                } else {
                    LazyColumn(
                        Modifier.fillMaxSize(),
                        contentPadding = androidx.compose.foundation.layout.PaddingValues(16.dp),
                        verticalArrangement = Arrangement.spacedBy(8.dp),
                    ) {
                        items(s.value, key = { it.id }) { site -> SiteRow(site) { onOpen(site.id) } }
                    }
                }
            }
        }
    }
}

@Composable
private fun SiteRow(s: Site, onClick: () -> Unit) {
    Card(onClick = onClick, modifier = Modifier.fillMaxWidth(), colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceContainer)) {
        Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
            Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.SpaceBetween) {
                Text(s.name, style = MaterialTheme.typography.titleMedium, color = MaterialTheme.colorScheme.onSurface, modifier = Modifier.weight(1f).padding(end = 8.dp))
                StatusChip(if (s.online) "Online" else "Offline", if (s.online) Tone.Good else Tone.Bad)
            }
            Text(
                "${s.accounts.total} flats: ${s.accounts.active} active, ${s.accounts.expired} expired, ${s.accounts.suspended} suspended",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            if (!s.online && s.cause.isNotBlank()) Text(s.cause, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.error)
            if (s.openAlerts > 0) StatusChip("${s.openAlerts} open alerts", Tone.Warn)
        }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun BuildingDetailScreen(
    id: String,
    onBack: () -> Unit,
    onNewDevices: (String) -> Unit,
    onOpenAccount: (String) -> Unit,
    onSetup: (String) -> Unit,
    onCutover: (String) -> Unit,
) {
    val container = LocalContainer.current
    val vm: BuildingDetailViewModel = viewModel(key = "building-$id", factory = vmFactory { BuildingDetailViewModel(id, container.sites) })
    val ui by vm.state.collectAsStateWithLifecycle()
    val title = (ui.site as? Loadable.Ready)?.value?.name ?: "Building"

    Scaffold(containerColor = MaterialTheme.colorScheme.background, topBar = { AppTopBar(title, onBack = onBack) }) { inner ->
        when (val s = ui.site) {
            Loadable.Loading -> CenteredLoading(Modifier.padding(inner))
            is Loadable.Failed -> ErrorState(s.message, vm::retry, Modifier.padding(inner))
            is Loadable.Ready -> PullToRefreshBox(ui.refreshing, vm::refresh, Modifier.padding(inner).fillMaxSize()) {
                LazyColumn(
                    Modifier.fillMaxSize(),
                    contentPadding = androidx.compose.foundation.layout.PaddingValues(16.dp),
                    verticalArrangement = Arrangement.spacedBy(12.dp),
                ) {
                    item { SiteSummary(s.value) }
                    item { Button(onClick = { onNewDevices(id) }, modifier = Modifier.fillMaxWidth()) { Text("New devices in this building") } }
                    item { Button(onClick = { onSetup(id) }, modifier = Modifier.fillMaxWidth()) { Text("Router setup checklist") } }
                    item { OutlinedButton(onClick = { onCutover(id) }, modifier = Modifier.fillMaxWidth()) { Text("Cutover to IPoE") } }
                    item { QoeSection(ui.qoe, ui.period, vm::setPeriod) }
                    item { Text("ONTs", style = MaterialTheme.typography.titleMedium, color = MaterialTheme.colorScheme.onBackground) }
                    when (val o = ui.onts) {
                        Loadable.Loading -> item { Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.Center) { LoadingSpinner() } }
                        is Loadable.Failed -> item { Text(o.message, color = MaterialTheme.colorScheme.error) }
                        is Loadable.Ready -> if (o.value.isEmpty()) {
                            item { Text("No ONTs configured.", color = MaterialTheme.colorScheme.onSurfaceVariant) }
                        } else {
                            items(o.value, key = { it.accountId + it.mac }) { ont -> OntRow(ont) { onOpenAccount(ont.accountId) } }
                        }
                    }
                }
            }
        }
    }
}

@Composable
private fun SiteSummary(s: Site) {
    SectionCard(s.name) {
        Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            StatusChip(if (s.online) "Online" else "Offline", if (s.online) Tone.Good else Tone.Bad)
            StatusChip(
                when (s.accessMode) {
                    "ipoe" -> "IPoE live"
                    "ipoe_observe" -> "IPoE observing"
                    else -> "Hotspot"
                },
                Tone.Neutral,
            )
        }
        if (!s.online && s.cause.isNotBlank()) Text(s.cause, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall)
        LabeledValue("Flats", "${s.accounts.total} (${s.accounts.active} active, ${s.accounts.expired} expired, ${s.accounts.suspended} suspended)")
        s.tunnel.handshakeAgeS?.let { LabeledValue("Tunnel handshake", "$it s ago") }
        s.uptime30dPct?.let { LabeledValue("Uptime, 30 days", String.format(Locale.US, "%.2f%%", it)) }
        LabeledValue("Open alerts", s.openAlerts.toString())
        s.provision?.let { p ->
            Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                StatusChip(if (p.inSync) "Router in sync" else "${p.drift} drifted", if (p.inSync) Tone.Good else Tone.Warn)
                if (p.pendingOps > 0) StatusChip("${p.pendingOps} pending", Tone.Neutral)
            }
            LabeledValue("Last provisioning run", formatAge(p.lastRunAt))
            if (p.lastError.isNotBlank()) Text(p.lastError, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall)
            p.foreignConflicts.forEach { Text("Router has a foreign entry: $it", color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall) }
        }
    }
}

@Composable
private fun QoeSection(qoe: Loadable<Qoe>, period: String, onPeriod: (String) -> Unit) {
    SectionCard("Quality of experience") {
        ChipRow(horizontalPadding = 0.dp) {
            QOE_PERIODS.forEach { p -> FilterChip(selected = period == p, onClick = { onPeriod(p) }, label = { ChipLabel(p) }) }
        }
        when (qoe) {
            Loadable.Loading -> Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.Center) { LoadingSpinner() }
            is Loadable.Failed -> Text(qoe.message, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall)
            is Loadable.Ready -> {
                val q = qoe.value
                LabeledValue("Latency", String.format(Locale.US, "%.1f ms", q.latencyMs))
                LabeledValue("Packet loss", String.format(Locale.US, "%.2f%%", q.lossPct))
                LabeledValue("Download / upload now", "${formatBps(q.wan.downBps)} / ${formatBps(q.wan.upBps)}")
                if (q.wan.capMbps > 0) {
                    LabeledValue("Uplink capacity", "${q.wan.capMbps} Mbps (peak ${String.format(Locale.US, "%.0f", q.wan.peakUtilPct)}%)")
                }
                if (q.series.downBps.size > 1) {
                    Text("Download", style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                    Sparkline(q.series.downBps.map { it.toDouble() })
                }
                if (q.series.rttMs.size > 1) {
                    Text("Latency", style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                    Sparkline(q.series.rttMs)
                }
                q.flags.forEach { f ->
                    Text(f.text, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurface)
                }
            }
        }
    }
}

@Composable
fun Sparkline(values: List<Double>, modifier: Modifier = Modifier) {
    val color = MaterialTheme.colorScheme.primary
    Canvas(modifier.fillMaxWidth().height(48.dp)) {
        if (values.size < 2) return@Canvas
        val max = values.max()
        val min = values.min()
        val span = (max - min).takeIf { it > 0 } ?: 1.0
        val path = Path()
        values.forEachIndexed { i, v ->
            val x = size.width * i / (values.size - 1)
            val y = size.height - (((v - min) / span).toFloat() * size.height)
            if (i == 0) path.moveTo(x, y) else path.lineTo(x, y)
        }
        drawPath(path, color, style = Stroke(width = 3.dp.toPx(), cap = StrokeCap.Round))
        drawLine(color.copy(alpha = 0.3f), Offset(0f, size.height), Offset(size.width, size.height), strokeWidth = 1.dp.toPx())
    }
}

@Composable
private fun OntRow(o: Ont, onClick: () -> Unit) {
    Card(onClick = onClick, modifier = Modifier.fillMaxWidth(), colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceContainer)) {
        Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(2.dp)) {
            Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.SpaceBetween) {
                Text("Flat ${o.flat}", style = MaterialTheme.typography.titleSmall, color = MaterialTheme.colorScheme.onSurface, maxLines = 1, modifier = Modifier.weight(1f).padding(end = 8.dp))
                Row(horizontalArrangement = Arrangement.spacedBy(6.dp)) {
                    if (o.suspendedOnRouter) StatusChip("Walled", Tone.Bad)
                    StatusChip(
                        o.leaseStatus.replaceFirstChar { it.uppercase() },
                        when (o.leaseStatus) {
                            "bound" -> Tone.Good
                            "waiting" -> Tone.Warn
                            else -> Tone.Neutral
                        },
                    )
                }
            }
            Text(
                listOf(o.mac, o.ip, o.hostName).filter { it.isNotBlank() }.joinToString("  |  "),
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            Text(
                "Seen ${formatAge(o.lastSeen)}  |  down ${formatBps(o.downBps)}  up ${formatBps(o.upBps)}",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
    }
}
