package ke.co.ftw.ftnadmin.ui.alerts

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.pulltorefresh.PullToRefreshBox
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.compose.ui.Modifier
import androidx.compose.ui.res.stringResource
import ke.co.ftw.ftnadmin.R
import ke.co.ftw.ftnadmin.ui.common.alertStateLabel
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import ke.co.ftw.ftnadmin.core.network.UserInfo
import ke.co.ftw.ftnadmin.domain.Alert
import ke.co.ftw.ftnadmin.domain.AlertMute
import ke.co.ftw.ftnadmin.ui.common.AppTopBar
import ke.co.ftw.ftnadmin.ui.common.CenteredLoading
import ke.co.ftw.ftnadmin.ui.common.ErrorState
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.common.LocalContainer
import ke.co.ftw.ftnadmin.ui.common.StatusChip
import ke.co.ftw.ftnadmin.ui.common.Tone
import ke.co.ftw.ftnadmin.ui.common.formatAge
import ke.co.ftw.ftnadmin.ui.common.formatDateTime
import ke.co.ftw.ftnadmin.ui.common.vmFactory

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun AlertsScreen(user: UserInfo, onBack: () -> Unit) {
    val container = LocalContainer.current
    val vm: AlertsViewModel = viewModel(factory = vmFactory { AlertsViewModel(container.alerts) })
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
        topBar = { AppTopBar("Alerts", onBack = onBack) },
        snackbarHost = { SnackbarHost(snack) },
    ) { inner ->
        when (val a = ui.alerts) {
            Loadable.Loading -> CenteredLoading(Modifier.padding(inner))
            is Loadable.Failed -> ErrorState(a.message, vm::retry, Modifier.padding(inner))
            is Loadable.Ready -> PullToRefreshBox(ui.refreshing, vm::refresh, Modifier.padding(inner).fillMaxSize()) {
                LazyColumn(
                    Modifier.fillMaxSize(),
                    contentPadding = androidx.compose.foundation.layout.PaddingValues(16.dp),
                    verticalArrangement = Arrangement.spacedBy(8.dp),
                ) {
                    item { Text(stringResource(R.string.active), style = MaterialTheme.typography.titleMedium, color = MaterialTheme.colorScheme.onBackground) }
                    if (a.value.active.isEmpty()) {
                        item { Text(stringResource(R.string.nothing_wrong), color = MaterialTheme.colorScheme.onSurfaceVariant) }
                    }
                    items(a.value.active, key = { "a-" + it.key }) { al ->
                        AlertCard(al, muted = a.value.mutes.any { it.routerKey == al.routerKey }, canMute = user.canWrite) { vm.openMute(al.routerKey, al.routerLabel) }
                    }
                    if (a.value.mutes.isNotEmpty()) {
                        item { Text(stringResource(R.string.muted_routers), style = MaterialTheme.typography.titleMedium, color = MaterialTheme.colorScheme.onBackground) }
                        val names = (a.value.active + a.value.history).associate { it.routerKey to it.routerLabel }
                        items(a.value.mutes, key = { "m-" + it.routerKey }) { m ->
                            MuteRow(m, names[m.routerKey]?.ifBlank { null } ?: m.routerKey, user.canWrite) { vm.mute(m.routerKey, 0) }
                        }
                    }
                    if (a.value.history.isNotEmpty()) {
                        item { Text(stringResource(R.string.recent_history), style = MaterialTheme.typography.titleMedium, color = MaterialTheme.colorScheme.onBackground) }
                        items(a.value.history, key = { "h-" + it.key + it.since }) { al -> AlertCard(al, muted = false, canMute = false) {} }
                    }
                }
            }
        }
    }

    ui.muteSite?.let { (siteId, name) ->
        AlertDialog(
            onDismissRequest = vm::dismissMute,
            title = { Text(stringResource(R.string.mute_alerts_for, name.ifBlank { stringResource(R.string.this_router) })) },
            text = {
                Column {
                    MUTE_CHOICES.forEach { (min, label) ->
                        TextButton(onClick = { vm.mute(siteId, min) }, enabled = !ui.busy) { Text(label) }
                    }
                }
            },
            confirmButton = {},
            dismissButton = { TextButton(onClick = vm::dismissMute) { Text(stringResource(R.string.action_cancel)) } },
        )
    }
}

@Composable
private fun AlertCard(a: Alert, muted: Boolean, canMute: Boolean, onMute: () -> Unit) {
    Card(modifier = Modifier.fillMaxWidth(), colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceContainer)) {
        Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
            Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.spacedBy(8.dp), verticalAlignment = androidx.compose.ui.Alignment.Top) {
                Text(a.label.ifBlank { a.rule }, style = MaterialTheme.typography.titleSmall, color = MaterialTheme.colorScheme.onSurface, modifier = Modifier.weight(1f))
                if (muted) StatusChip(stringResource(R.string.muted), Tone.Neutral, Modifier.weight(1f, fill = false)) else StatusChip(alertStateLabel(a.state.ifBlank { "open" }), Tone.Warn, Modifier.weight(1f, fill = false))
            }
            if (a.routerLabel.isNotBlank()) Text(a.routerLabel, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurface)
            if (a.detail.isNotBlank()) Text(a.detail, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            Text(stringResource(R.string.since_age, formatDateTime(a.since), formatAge(a.since)), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            if (canMute && a.routerKey.isNotBlank() && !muted) TextButton(onClick = onMute) { Text(stringResource(R.string.mute_this_router)) }
        }
    }
}

@Composable
private fun MuteRow(m: AlertMute, name: String, canUnmute: Boolean, onUnmute: () -> Unit) {
    Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.SpaceBetween, verticalAlignment = androidx.compose.ui.Alignment.CenterVertically) {
        // The text takes the room the button leaves, so Unmute is never squeezed out by a long router name.
        Text(
            "$name until ${formatDateTime(m.until)}",
            color = MaterialTheme.colorScheme.onSurface,
            style = MaterialTheme.typography.bodyMedium,
            modifier = Modifier.weight(1f).padding(end = 8.dp),
        )
        if (canUnmute) TextButton(onClick = onUnmute) { Text(stringResource(R.string.unmute)) }
    }
}
