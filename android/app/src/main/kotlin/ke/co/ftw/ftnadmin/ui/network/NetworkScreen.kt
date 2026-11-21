package ke.co.ftw.ftnadmin.ui.network

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Clear
import androidx.compose.material.icons.filled.Search
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FilterChip
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Text
import androidx.compose.material3.pulltorefresh.PullToRefreshBox
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import ke.co.ftw.ftnadmin.core.network.UserInfo
import ke.co.ftw.ftnadmin.ui.common.CenteredLoading
import ke.co.ftw.ftnadmin.ui.common.ChipLabel
import ke.co.ftw.ftnadmin.ui.common.ChipRow
import ke.co.ftw.ftnadmin.ui.common.EmptyState
import ke.co.ftw.ftnadmin.ui.common.ErrorState
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.common.LocalContainer
import ke.co.ftw.ftnadmin.ui.common.StatusDot
import ke.co.ftw.ftnadmin.ui.common.TabScaffold
import ke.co.ftw.ftnadmin.ui.common.vmFactory

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun NetworkScreen(
    user: UserInfo,
    onAlerts: () -> Unit,
    onOpenCustomer: (String) -> Unit,
    onCreateCustomer: (routerId: String, mac: String) -> Unit,
) {
    val container = LocalContainer.current
    val vm: NetworkViewModel = viewModel(
        factory = vmFactory {
            NetworkViewModel(container.routerLive, container.accounts, container.newDevices, container.routerContext.selection)
        },
    )
    val ui by vm.state.collectAsStateWithLifecycle()
    val snack = remember { SnackbarHostState() }
    LaunchedEffect(ui.message) {
        ui.message?.let {
            snack.showSnackbar(it)
            vm.clearMessage()
        }
    }

    TabScaffold(title = "Network", onAlerts = onAlerts, snackbarHost = { SnackbarHost(snack) }) { inner ->
        val id = ui.routerId
        if (id == null) {
            PickRouter(Modifier.padding(inner))
        } else {
            Column(Modifier.padding(inner).fillMaxSize()) {
                ChipRow(Modifier.padding(bottom = 4.dp)) {
                    NetworkTab.entries.forEach { t ->
                        FilterChip(selected = ui.tab == t, onClick = { vm.setTab(t) }, label = { ChipLabel(t.label) })
                    }
                }
                when (ui.tab) {
                    NetworkTab.Clients -> ClientsPane(ui, vm, user, id, onOpenCustomer, onCreateCustomer)
                    NetworkTab.Interfaces -> InterfacesPane(ui, vm)
                }
            }
        }
    }
}

/** "All routers" is selected: the network views are per router, so offer the routers to pick from. */
@Composable
private fun PickRouter(modifier: Modifier) {
    val ctx = LocalContainer.current.routerContext
    val routers by ctx.routers.collectAsStateWithLifecycle()
    Column(modifier.fillMaxSize().padding(16.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
        Text(
            "Pick a router",
            style = MaterialTheme.typography.titleLarge,
            fontWeight = FontWeight.ExtraBold,
            color = MaterialTheme.colorScheme.onBackground,
        )
        Text(
            "Clients and interfaces are shown one router at a time. You can also switch router from the top bar.",
            style = MaterialTheme.typography.bodyMedium,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
        val list = (routers as? Loadable.Ready)?.value.orEmpty()
        list.forEach { r ->
            androidx.compose.material3.Card(
                onClick = { ctx.select(r.key) },
                modifier = Modifier.fillMaxWidth(),
                colors = androidx.compose.material3.CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceContainerHigh),
            ) {
                Row(Modifier.padding(16.dp).fillMaxWidth(), horizontalArrangement = Arrangement.spacedBy(12.dp), verticalAlignment = androidx.compose.ui.Alignment.CenterVertically) {
                    StatusDot(r.online, size = 12.dp)
                    Text(r.label, style = MaterialTheme.typography.titleMedium, fontWeight = FontWeight.Bold, maxLines = 1, modifier = Modifier.weight(1f))
                }
            }
        }
        if (list.isEmpty()) Text("No routers yet.", color = MaterialTheme.colorScheme.onSurfaceVariant)
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun ClientsPane(
    ui: NetworkUiState,
    vm: NetworkViewModel,
    user: UserInfo,
    routerId: String,
    onOpenCustomer: (String) -> Unit,
    onCreateCustomer: (String, String) -> Unit,
) {
    OutlinedTextField(
        value = ui.query,
        onValueChange = vm::onQuery,
        placeholder = { Text("Search name, house number, IP, MAC or vendor", maxLines = 1) },
        leadingIcon = { Icon(Icons.Default.Search, contentDescription = null) },
        trailingIcon = {
            if (ui.query.isNotEmpty()) {
                IconButton(onClick = { vm.onQuery("") }) { Icon(Icons.Default.Clear, contentDescription = "Clear search") }
            }
        },
        singleLine = true,
        modifier = Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 4.dp),
    )
    ChipRow(Modifier.padding(vertical = 4.dp)) {
        ClientFilter.entries.forEach { f ->
            val count = (ui.clients as? Loadable.Ready)?.value?.counts?.let {
                when (f) {
                    ClientFilter.Flats -> it.flats
                    ClientFilter.Unregistered -> it.unregistered
                    ClientFilter.Online -> it.online
                    ClientFilter.All -> null
                }
            }
            FilterChip(
                selected = ui.filter == f,
                onClick = { vm.setFilter(f) },
                label = { ChipLabel(if (count != null) "${f.label} $count" else f.label) },
            )
        }
    }
    when (val c = ui.clients) {
        Loadable.Loading -> CenteredLoading()
        is Loadable.Failed -> ErrorState(c.message, vm::retry)
        is Loadable.Ready -> PullToRefreshBox(ui.refreshing, vm::refresh, Modifier.fillMaxSize()) {
            if (c.value.items.isEmpty()) {
                EmptyState(if (ui.query.isBlank() && ui.filter == ClientFilter.All) "No devices on this router yet." else "Nothing matches.")
            } else {
                LazyColumn(
                    Modifier.fillMaxSize(),
                    contentPadding = PaddingValues(start = 16.dp, end = 16.dp, top = 4.dp, bottom = 24.dp),
                    verticalArrangement = Arrangement.spacedBy(8.dp),
                ) {
                    items(c.value.items, key = { it.key }) { cl ->
                        ClientRow(
                            cl, expanded = ui.expanded == cl.key, busy = ui.busy == cl.key, canWrite = user.canWrite,
                            onToggle = { vm.toggle(cl.key) },
                            onOpen = { onOpenCustomer(cl.accountId) },
                            onSuspend = { vm.suspendToggle(cl) },
                            onResync = { vm.resync(cl) },
                            onCreate = { onCreateCustomer(routerId, cl.mac) },
                            onDismiss = { vm.dismiss(cl) },
                        )
                    }
                }
            }
        }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun InterfacesPane(ui: NetworkUiState, vm: NetworkViewModel) {
    when (val l = ui.interfaces) {
        Loadable.Loading -> CenteredLoading()
        is Loadable.Failed -> ErrorState(l.message, vm::retry)
        is Loadable.Ready -> PullToRefreshBox(ui.refreshing, vm::refresh, Modifier.fillMaxSize()) {
            if (l.value.isEmpty()) {
                EmptyState("No interfaces reported.")
            } else {
                LazyColumn(
                    Modifier.fillMaxSize(),
                    contentPadding = PaddingValues(start = 16.dp, end = 16.dp, top = 4.dp, bottom = 24.dp),
                    verticalArrangement = Arrangement.spacedBy(8.dp),
                ) {
                    items(l.value, key = { it.name }) { i ->
                        InterfaceRow(i, expanded = ui.expanded == "i-${i.name}", onToggle = { vm.toggle("i-${i.name}") })
                    }
                }
            }
        }
    }
}
