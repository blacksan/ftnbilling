package ke.co.ftw.ftnadmin.ui.dashboard

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.pulltorefresh.PullToRefreshBox
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import ke.co.ftw.ftnadmin.domain.Dashboard
import ke.co.ftw.ftnadmin.ui.common.AppTopBar
import ke.co.ftw.ftnadmin.ui.common.CenteredLoading
import ke.co.ftw.ftnadmin.ui.common.ErrorState
import ke.co.ftw.ftnadmin.ui.common.LabeledValue
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.common.LocalContainer
import ke.co.ftw.ftnadmin.ui.common.SectionCard
import ke.co.ftw.ftnadmin.ui.common.StatusChip
import ke.co.ftw.ftnadmin.ui.common.Tone
import ke.co.ftw.ftnadmin.ui.common.formatAge
import ke.co.ftw.ftnadmin.ui.common.formatKes
import ke.co.ftw.ftnadmin.ui.common.vmFactory

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun DashboardScreen(
    onSettings: () -> Unit,
    onNewDevices: () -> Unit,
    onUnmatchedPayments: () -> Unit,
    onAlerts: () -> Unit,
) {
    val container = LocalContainer.current
    val vm: DashboardViewModel = viewModel(factory = vmFactory { DashboardViewModel(container.dashboard) })
    val ui by vm.state.collectAsStateWithLifecycle()

    Scaffold(
        containerColor = MaterialTheme.colorScheme.background,
        topBar = { AppTopBar("Fast Tech Net", onSettings = onSettings) },
    ) { inner ->
        when (val d = ui.data) {
            Loadable.Loading -> CenteredLoading(Modifier.padding(inner))
            is Loadable.Failed -> ErrorState(d.message, vm::load, Modifier.padding(inner))
            is Loadable.Ready -> PullToRefreshBox(
                isRefreshing = ui.refreshing,
                onRefresh = vm::refresh,
                modifier = Modifier.padding(inner).fillMaxSize(),
            ) {
                DashboardContent(d.value, ui.staleError, onNewDevices, onUnmatchedPayments, onAlerts)
            }
        }
    }
}

@Composable
private fun DashboardContent(
    d: Dashboard,
    staleError: String?,
    onNewDevices: () -> Unit,
    onUnmatchedPayments: () -> Unit,
    onAlerts: () -> Unit,
) {
    Column(
        Modifier.fillMaxSize().verticalScroll(rememberScrollState()).padding(16.dp),
        verticalArrangement = Arrangement.spacedBy(12.dp),
    ) {
        staleError?.let { Text("Could not refresh: $it", color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall) }

        // Things that need the operator's attention come first.
        val attention = listOf(
            Triple("New devices", d.unregisteredDevices, onNewDevices),
            Triple("Unmatched payments", d.unmatchedPayments, onUnmatchedPayments),
            Triple("Open alerts", d.openAlerts, onAlerts),
        )
        Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            attention.forEach { (label, n, click) ->
                AttentionTile(label, n, click, Modifier.weight(1f))
            }
        }

        SectionCard("Revenue") {
            LabeledValue("Today", formatKes(d.revenue.todayKes))
            LabeledValue("This month", formatKes(d.revenue.monthKes))
            LabeledValue("Last month", formatKes(d.revenue.lastMonthKes))
        }
        SectionCard("Customers") {
            LabeledValue("Active", d.accounts.active.toString())
            LabeledValue("Expired", d.accounts.expired.toString())
            LabeledValue("Suspended", d.accounts.suspended.toString())
            LabeledValue("New (no plan yet)", d.accounts.new.toString())
            if (d.accounts.macConflict > 0) {
                Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    StatusChip("${d.accounts.macConflict} MAC conflicts", Tone.Bad)
                    Text("Fix these on the Customers tab.", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                }
            }
        }
        SectionCard("Network") {
            Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                StatusChip("${d.sitesOnline} of ${d.sitesTotal} buildings online", if (d.sitesOnline == d.sitesTotal) Tone.Good else Tone.Warn)
            }
            LabeledValue("Router tasks waiting", d.pendingRouterTasks.toString())
        }
        Text(
            "Updated ${formatAge(d.generatedAt)}",
            style = MaterialTheme.typography.labelSmall,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
    }
}

@Composable
private fun AttentionTile(label: String, count: Int, onClick: () -> Unit, modifier: Modifier = Modifier) {
    val hot = count > 0
    Card(
        onClick = onClick,
        modifier = modifier,
        colors = CardDefaults.cardColors(
            containerColor = if (hot) MaterialTheme.colorScheme.primaryContainer else MaterialTheme.colorScheme.surfaceContainer,
            contentColor = if (hot) MaterialTheme.colorScheme.onPrimaryContainer else MaterialTheme.colorScheme.onSurface,
        ),
    ) {
        Column(Modifier.padding(12.dp).fillMaxWidth()) {
            Text(count.toString(), style = MaterialTheme.typography.headlineMedium)
            Text(label, style = MaterialTheme.typography.labelMedium)
        }
    }
}
