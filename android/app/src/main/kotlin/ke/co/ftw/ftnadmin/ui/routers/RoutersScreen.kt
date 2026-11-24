package ke.co.ftw.ftnadmin.ui.routers

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.KeyboardArrowRight
import androidx.compose.material.icons.filled.Add
import androidx.compose.material3.Button
import androidx.compose.material3.ExtendedFloatingActionButton
import androidx.compose.material3.Icon
import androidx.compose.material3.ListItem
import androidx.compose.material3.ListItemDefaults
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
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.setValue
import androidx.compose.ui.platform.LocalContext
import android.content.ActivityNotFoundException
import android.content.Intent
import android.net.Uri
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.res.pluralStringResource
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import ke.co.ftw.ftnadmin.R
import ke.co.ftw.ftnadmin.data.sites.SetupState
import ke.co.ftw.ftnadmin.ui.common.MonoText
import ke.co.ftw.ftnadmin.ui.common.houseText
import ke.co.ftw.ftnadmin.ui.common.leaseLabel
import ke.co.ftw.ftnadmin.ui.common.onlineStateRes
import ke.co.ftw.ftnadmin.ui.routers.setup.BootstrapLineCard
import ke.co.ftw.ftnadmin.ui.routers.setup.CopyButton
import ke.co.ftw.ftnadmin.ui.routers.setup.ResetRouterCard
import ke.co.ftw.ftnadmin.ui.routers.setup.ResetRouterDialog
import ke.co.ftw.ftnadmin.ui.routers.setup.RouterSetupViewModel
import ke.co.ftw.ftnadmin.ui.routers.setup.SetupStepsCard
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.remember
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.LifecycleEventObserver
import androidx.lifecycle.compose.LocalLifecycleOwner
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
import ke.co.ftw.ftnadmin.ui.common.Sparkline
import ke.co.ftw.ftnadmin.core.network.UserInfo
import ke.co.ftw.ftnadmin.ui.common.StatusChip
import ke.co.ftw.ftnadmin.ui.common.Tone
import ke.co.ftw.ftnadmin.ui.common.formatAge
import ke.co.ftw.ftnadmin.ui.common.formatBps
import ke.co.ftw.ftnadmin.ui.common.vmFactory
import java.util.Locale

/**
 * The Routers tab of Equipment: the router list of [vm], pull to refresh. The Equipment screen owns the top bar and the
 * "Add router" button (it changes with the tab), so this is only the list. [contentPadding] is the room under the FAB.
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun RoutersTab(vm: RoutersViewModel, onOpen: (String) -> Unit, modifier: Modifier = Modifier) {
    val ui by vm.state.collectAsStateWithLifecycle()
    when (val s = ui.sites) {
        Loadable.Loading -> CenteredLoading(modifier)
        is Loadable.Failed -> ErrorState(s.message, vm::retry, modifier)
        is Loadable.Ready -> PullToRefreshBox(ui.refreshing, vm::refresh, modifier.fillMaxSize()) {
            if (s.value.isEmpty()) {
                EmptyState(stringResource(R.string.no_routers_linked))
            } else {
                LazyColumn(
                    Modifier.fillMaxSize(),
                    contentPadding = androidx.compose.foundation.layout.PaddingValues(start = 16.dp, end = 16.dp, top = 8.dp, bottom = 96.dp),
                    verticalArrangement = Arrangement.spacedBy(10.dp),
                ) {
                    items(s.value, key = { it.key }) { site -> RouterRow(site) { onOpen(site.key) } }
                }
            }
        }
    }
}

@Composable
private fun RouterRow(s: Site, onClick: () -> Unit) {
    Card(onClick = onClick, modifier = Modifier.fillMaxWidth(), colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceContainerHigh)) {
        Column(Modifier.padding(14.dp), verticalArrangement = Arrangement.spacedBy(6.dp)) {
            Row(Modifier.fillMaxWidth(), verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(10.dp)) {
                // The chip says Online or Offline in words, so there is no separate status dot here.
                Text(s.label, style = MaterialTheme.typography.titleMedium, color = MaterialTheme.colorScheme.onSurface, maxLines = 2, overflow = TextOverflow.Ellipsis, modifier = Modifier.weight(1f))
                StatusChip(stringResource(onlineStateRes(s.online)), if (s.online) Tone.Good else Tone.Bad, Modifier.weight(1f, fill = false))
            }
            Text(
                stringResource(R.string.houses_summary, pluralStringResource(R.plurals.houses_count, s.accounts.total, s.accounts.total), s.accounts.active, s.accounts.expired, s.accounts.suspended),
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            if (!s.online && s.cause.isNotBlank()) Text(s.cause, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.error)
            if (s.openAlerts > 0 || s.setupState.isNotBlank()) {
                ChipRow(horizontalPadding = 0.dp) {
                    if (s.openAlerts > 0) StatusChip(pluralStringResource(R.plurals.open_alerts, s.openAlerts, s.openAlerts), Tone.Warn)
                    if (s.setupState.isNotBlank() && s.setupState != SetupState.READY) StatusChip(stringResource(R.string.setup_state_chip, SetupState.label(s.setupState)), Tone.Neutral)
                }
            }
        }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun RouterDetailScreen(
    id: String,
    user: UserInfo,
    onBack: () -> Unit,
    onNewDevices: (String) -> Unit,
    onOpenAccount: (String) -> Unit,
    onChecks: (String) -> Unit,
    onPorts: (String, String) -> Unit,
    onEdit: (String) -> Unit,
    onOnus: (String) -> Unit = {},
) {
    val container = LocalContainer.current
    val vm: RouterDetailViewModel = viewModel(key = "router-$id", factory = vmFactory { RouterDetailViewModel(id, container.sites) })
    val setupVm: RouterSetupViewModel = viewModel(
        key = "router-setup-$id",
        factory = vmFactory { RouterSetupViewModel(id, container.siteSetup, container.sites, user) },
    )
    val ui by vm.state.collectAsStateWithLifecycle()
    val setupUi by setupVm.state.collectAsStateWithLifecycle()
    val title = (ui.site as? Loadable.Ready)?.value?.label ?: stringResource(R.string.router)
    val snack = remember { SnackbarHostState() }
    LaunchedEffect(setupUi.message) {
        setupUi.message?.let {
            snack.showSnackbar(it)
            setupVm.clearMessage()
        }
    }
    // Poll the setup steps only while this page is on screen.
    val lifecycleOwner = LocalLifecycleOwner.current
    DisposableEffect(lifecycleOwner, setupVm) {
        val observer = LifecycleEventObserver { _, event ->
            when (event) {
                Lifecycle.Event.ON_START -> setupVm.resume()
                Lifecycle.Event.ON_STOP -> setupVm.pause()
                else -> Unit
            }
        }
        lifecycleOwner.lifecycle.addObserver(observer)
        onDispose {
            lifecycleOwner.lifecycle.removeObserver(observer)
            setupVm.pause()
        }
    }

    Scaffold(
        containerColor = MaterialTheme.colorScheme.background,
        topBar = { AppTopBar(title, onBack = onBack) },
        snackbarHost = { SnackbarHost(snack) },
    ) { inner ->
        when (val s = ui.site) {
            Loadable.Loading -> CenteredLoading(Modifier.padding(inner))
            is Loadable.Failed -> ErrorState(s.message, vm::retry, Modifier.padding(inner))
            is Loadable.Ready -> PullToRefreshBox(
                isRefreshing = ui.refreshing || setupUi.refreshing,
                onRefresh = {
                    vm.refresh()
                    setupVm.refresh()
                },
                modifier = Modifier.padding(inner).fillMaxSize(),
            ) {
                LazyColumn(
                    Modifier.fillMaxSize(),
                    contentPadding = androidx.compose.foundation.layout.PaddingValues(16.dp),
                    verticalArrangement = Arrangement.spacedBy(12.dp),
                ) {
                    item { SiteSummary(s.value) }
                    webfigLaunchUrl(s.value.webfigUrl, user.isOwner)?.let { url -> item { OpenWebFigCard(url) } }
                    item { SetupStepsCard(setupUi, onRetry = setupVm::retry, onChecks = { onChecks(id) }) }
                    item { BootstrapLineCard(setupUi, user.isOwner, onGet = setupVm::getLine, onHide = setupVm::hideLine, wanInterface = s.value.wanInterface, lanPorts = s.value.lanPorts) }
                    // Where to go from here: list rows with a chevron, not a stack of filled buttons. The one entry to the
                    // checks is "Open checks" inside the Setup card above.
                    item {
                        Card(colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceContainer)) {
                            Column {
                                NavRow(stringResource(R.string.new_devices_on_router), stringResource(R.string.new_devices_on_router_sub)) { onNewDevices(id) }
                                NavRow(stringResource(R.string.ports), stringResource(R.string.ports_sub)) { onPorts(id, s.value.label) }
                                NavRow(stringResource(R.string.onus_nav), stringResource(R.string.onus_nav_sub)) { onOnus(id) }
                                if (user.isOwner) NavRow(stringResource(R.string.router_settings), stringResource(R.string.router_settings_sub)) { onEdit(id) }
                            }
                        }
                    }
                    item { QoeSection(ui.qoe, ui.period, vm::setPeriod) }
                    item { Text(stringResource(R.string.onus), style = MaterialTheme.typography.titleMedium, color = MaterialTheme.colorScheme.onBackground) }
                    when (val o = ui.onts) {
                        Loadable.Loading -> item { Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.Center) { LoadingSpinner() } }
                        is Loadable.Failed -> item { Text(o.message, color = MaterialTheme.colorScheme.error) }
                        is Loadable.Ready -> if (o.value.isEmpty()) {
                            item { Text(stringResource(R.string.no_onus_configured), color = MaterialTheme.colorScheme.onSurfaceVariant) }
                        } else {
                            items(o.value, key = { it.accountId + it.mac }) { ont -> OntRow(ont) { onOpenAccount(ont.accountId) } }
                        }
                    }
                    if (user.isOwner) item { ResetRouterCard(onReset = setupVm::openReset) }
                }
            }
        }
    }

    if (setupUi.resetDialog) {
        ResetRouterDialog(
            ui = setupUi,
            canReset = setupVm.canReset(),
            onTyped = setupVm::setTypedName,
            onConfirm = setupVm::reset,
            onClose = setupVm::closeReset,
        )
    }
}

/** What the router page says about remote WinBox, or null when the router has no tunnel address yet. */
internal fun winboxNotice(tunnelAddress: String, mgmtWinbox: Boolean): String? = when {
    tunnelAddress.isBlank() -> null
    mgmtWinbox -> "Remote WinBox: connect your management WireGuard profile, then open WinBox to $tunnelAddress"
    else -> "No report that this router has remote WinBox. If it was set up before remote WinBox existed, reset it and paste a new line."
}

/**
 * The address to open in the system browser for "Open WebFig", or null: only owners get the button, and only for
 * an https address (the server sends its own admin page; anything else is never opened).
 */
internal fun webfigLaunchUrl(webfigUrl: String, isOwner: Boolean): String? =
    webfigUrl.takeIf { isOwner && it.startsWith("https://") }

@Composable
private fun OpenWebFigCard(url: String) {
    val context = LocalContext.current
    var failed by remember { mutableStateOf(false) }
    SectionCard("WebFig") {
        Text(
            "Opens the router's own web page through the server, in your browser. The browser asks for your password and authenticator code again.",
            style = MaterialTheme.typography.bodySmall,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
        Button(
            onClick = {
                val open = Intent(Intent.ACTION_VIEW, Uri.parse(url)).addCategory(Intent.CATEGORY_BROWSABLE)
                failed = try {
                    context.startActivity(open)
                    false
                } catch (_: ActivityNotFoundException) {
                    true
                }
            },
            modifier = Modifier.fillMaxWidth(),
        ) { Text("Open WebFig") }
        if (failed) Text("No browser app was found on this phone.", color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall)
    }
}

@Composable
private fun RemoteWinbox(s: Site) {
    if (s.tunnelAddress.isNotBlank()) LabeledValue(stringResource(R.string.tunnel_address), s.tunnelAddress, mono = true)
    val notice = winboxNotice(s.tunnelAddress, s.mgmtWinbox) ?: return
    Text(
        notice,
        style = MaterialTheme.typography.bodySmall,
        color = MaterialTheme.colorScheme.onSurfaceVariant,
    )
    if (s.mgmtWinbox) CopyButton("Copy address", s.tunnelAddress)
}

@Composable
private fun SiteSummary(s: Site) {
    SectionCard(s.label) {
        StatusChip(stringResource(onlineStateRes(s.online)), if (s.online) Tone.Good else Tone.Bad)
        if (!s.online && s.cause.isNotBlank()) Text(s.cause, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall)
        LabeledValue(stringResource(R.string.houses), stringResource(R.string.houses_breakdown, s.accounts.total, s.accounts.active, s.accounts.expired, s.accounts.suspended))
        s.tunnel.handshakeAgeS?.let { LabeledValue("Tunnel handshake", "$it s ago") }
        RemoteWinbox(s)
        s.uptime30dPct?.let { LabeledValue("Uptime, 30 days", String.format(Locale.US, "%.2f%%", it)) }
        LabeledValue("Open alerts", s.openAlerts.toString())
        s.provision?.let { p ->
            FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
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
            QOE_PERIODS.forEach { p -> FilterChip(selected = period == p, onClick = { onPeriod(p) }, label = { ChipLabel(qoePeriodLabel(p)) }) }
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

@OptIn(ExperimentalLayoutApi::class)
@Composable
private fun OntRow(o: Ont, onClick: () -> Unit) {
    Card(onClick = onClick, modifier = Modifier.fillMaxWidth(), colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceContainer)) {
        Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(2.dp)) {
            Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.spacedBy(8.dp), verticalAlignment = Alignment.Top) {
                Text(
                    houseText(o.unitLabel).ifBlank { o.code },
                    style = MaterialTheme.typography.titleSmall,
                    color = MaterialTheme.colorScheme.onSurface,
                    maxLines = 2,
                    overflow = TextOverflow.Ellipsis,
                    modifier = Modifier.weight(1f),
                )
                FlowRow(Modifier.weight(1f, fill = false), horizontalArrangement = Arrangement.spacedBy(6.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                    if (o.suspendedOnRouter) StatusChip(stringResource(R.string.walled), Tone.Bad)
                    StatusChip(
                        leaseLabel(o.leaseStatus),
                        when (o.leaseStatus) {
                            "bound" -> Tone.Good
                            "waiting" -> Tone.Warn
                            else -> Tone.Neutral
                        },
                    )
                }
            }
            if (o.mac.isNotBlank()) MonoText(o.mac, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            if (o.ip.isNotBlank()) MonoText(o.ip, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            if (o.hostName.isNotBlank()) Text(o.hostName, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            Text(
                stringResource(R.string.ont_traffic, formatAge(o.lastSeen), formatBps(o.downBps), formatBps(o.upBps)),
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
    }
}

/** A navigation row: title, one line of help, a chevron. The whole row is the touch target. */
@Composable
private fun NavRow(title: String, subtitle: String, onClick: () -> Unit) {
    ListItem(
        headlineContent = { Text(title) },
        supportingContent = { Text(subtitle) },
        trailingContent = { Icon(Icons.AutoMirrored.Filled.KeyboardArrowRight, contentDescription = null) },
        colors = ListItemDefaults.colors(containerColor = Color.Transparent),
        modifier = Modifier.clickable(role = Role.Button, onClick = onClick),
    )
}
