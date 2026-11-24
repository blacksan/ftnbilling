package ke.co.ftw.ftnadmin.ui.network

import androidx.compose.foundation.layout.Box
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
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
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
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import ke.co.ftw.ftnadmin.R
import ke.co.ftw.ftnadmin.core.network.UserInfo
import ke.co.ftw.ftnadmin.ui.common.ChipLabel
import ke.co.ftw.ftnadmin.ui.common.ChipRow
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.common.LoadingSpinner
import ke.co.ftw.ftnadmin.ui.common.LocalContainer
import ke.co.ftw.ftnadmin.ui.common.StatusDot
import ke.co.ftw.ftnadmin.ui.common.TabScaffold
import ke.co.ftw.ftnadmin.ui.common.onlineStateRes
import ke.co.ftw.ftnadmin.ui.common.vmFactory

/**
 * The Network screen: Map (Google Maps, one pin per apartment), List (the same data as cards, with Open in Google Maps)
 * and Live (the router-by-router clients and interfaces this screen showed before the map).
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun NetworkScreen(
    user: UserInfo,
    onAlerts: () -> Unit,
    onOpenCustomer: (String) -> Unit,
    onCreateCustomer: (routerId: String, mac: String) -> Unit,
    links: NetworkLinks,
    /** Opens the customer with the Edit MAC dialog up (the swap/replace flow for a changed ONU). */
    onChangeMac: (String) -> Unit = onOpenCustomer,
) {
    val container = LocalContainer.current
    val vm: NetworkViewModel = viewModel(
        factory = vmFactory {
            NetworkViewModel(container.routerLive, container.accounts, container.newDevices, container.routerContext.selection)
        },
    )
    val mapVm: NetworkMapViewModel = viewModel(factory = vmFactory { NetworkMapViewModel(container.networkMap) })
    val ui by vm.state.collectAsStateWithLifecycle()
    val mapUi by mapVm.state.collectAsStateWithLifecycle()
    val snack = remember { SnackbarHostState() }
    LaunchedEffect(ui.message) {
        ui.message?.let {
            snack.showSnackbar(it)
            vm.clearMessage()
        }
    }
    LaunchedEffect(mapUi.message) {
        mapUi.message?.let {
            snack.showSnackbar(it)
            mapVm.clearMessage()
        }
    }

    TabScaffold(title = stringResource(R.string.tab_network), onAlerts = onAlerts, snackbarHost = { SnackbarHost(snack) }) { inner ->
        Column(Modifier.padding(inner).fillMaxSize()) {
            ChipRow(Modifier.padding(bottom = 4.dp)) {
                NetworkMode.entries.forEach { m ->
                    FilterChip(selected = mapUi.mode == m, onClick = { mapVm.setMode(m) }, label = { ChipLabel(m.label) })
                }
            }
            when (mapUi.mode) {
                NetworkMode.Live -> LivePane(ui, vm, user, onOpenCustomer, onChangeMac, onCreateCustomer)
                else -> NetworkMapPane(mapUi, mapVm, user.canWrite, links)
            }
        }
    }
}

/** The router-by-router view (clients and interfaces of the selected router). */
@Composable
private fun LivePane(
    ui: NetworkUiState,
    vm: NetworkViewModel,
    user: UserInfo,
    onOpenCustomer: (String) -> Unit,
    onChangeMac: (String) -> Unit,
    onCreateCustomer: (routerId: String, mac: String) -> Unit,
) {
    val id = ui.routerId
    if (id == null) {
        PickRouter(Modifier)
    } else {
        // One scrolling list per pane: the tab chips, search and filters are its first rows, so at large font sizes
        // they scroll away instead of squeezing the list to nothing.
        Box(Modifier.fillMaxSize()) {
            when (ui.tab) {
                NetworkTab.Clients -> ClientsPane(ui, vm, user, id, onOpenCustomer, onChangeMac, onCreateCustomer)
                NetworkTab.Interfaces -> InterfacesPane(ui, vm)
            }
        }
    }
}

@Composable
private fun TabChips(ui: NetworkUiState, vm: NetworkViewModel) {
    ChipRow(Modifier.padding(bottom = 4.dp), horizontalPadding = 0.dp) {
        NetworkTab.entries.forEach { t ->
            FilterChip(selected = ui.tab == t, onClick = { vm.setTab(t) }, label = { ChipLabel(stringResource(t.labelRes)) })
        }
    }
}

/** Centered spinner or error inside a scrolling list (the list has no bounded height, so nothing here fills the screen). */
@Composable
private fun PaneMessage(content: @Composable () -> Unit) {
    Column(Modifier.fillMaxWidth().padding(vertical = 32.dp), horizontalAlignment = Alignment.CenterHorizontally, verticalArrangement = Arrangement.spacedBy(12.dp)) { content() }
}

/** "All routers" is selected: the network views are per router, so offer the routers to pick from. */
@Composable
private fun PickRouter(modifier: Modifier) {
    val ctx = LocalContainer.current.routerContext
    val routers by ctx.routers.collectAsStateWithLifecycle()
    val list = (routers as? Loadable.Ready)?.value.orEmpty()
    LazyColumn(modifier.fillMaxSize(), contentPadding = PaddingValues(16.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
        item {
            Column(verticalArrangement = Arrangement.spacedBy(12.dp)) {
                Text(
                    stringResource(R.string.pick_router),
                    style = MaterialTheme.typography.titleLarge,
                    color = MaterialTheme.colorScheme.onBackground,
                )
                Text(
                    stringResource(R.string.pick_router_hint),
                    style = MaterialTheme.typography.bodyMedium,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
        }
        items(list, key = { it.key }) { r ->
            Card(
                onClick = { ctx.select(r.key) },
                modifier = Modifier.fillMaxWidth(),
                colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceContainerHigh),
            ) {
                Row(Modifier.padding(16.dp).fillMaxWidth(), horizontalArrangement = Arrangement.spacedBy(12.dp), verticalAlignment = Alignment.CenterVertically) {
                    StatusDot(r.online, size = 12.dp, describe = false)
                    Text(r.label, style = MaterialTheme.typography.titleMedium, maxLines = 2, overflow = TextOverflow.Ellipsis, modifier = Modifier.weight(1f))
                    Text(stringResource(onlineStateRes(r.online)), style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
                }
            }
        }
        if (list.isEmpty()) item { Text(stringResource(R.string.no_routers_yet), color = MaterialTheme.colorScheme.onSurfaceVariant) }
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
    onChangeMac: (String) -> Unit,
    onCreateCustomer: (String, String) -> Unit,
) {
    PullToRefreshBox(ui.refreshing, vm::refresh, Modifier.fillMaxSize()) {
        LazyColumn(
            Modifier.fillMaxSize(),
            contentPadding = PaddingValues(start = 16.dp, end = 16.dp, top = 0.dp, bottom = 24.dp),
            verticalArrangement = Arrangement.spacedBy(8.dp),
        ) {
            item { TabChips(ui, vm) }
            item {
                OutlinedTextField(
                    value = ui.query,
                    onValueChange = vm::onQuery,
                    placeholder = { Text(stringResource(R.string.search_clients), maxLines = 1, overflow = TextOverflow.Ellipsis) },
                    supportingText = { Text(stringResource(R.string.search_clients_hint)) },
                    leadingIcon = { Icon(Icons.Default.Search, contentDescription = null) },
                    trailingIcon = {
                        if (ui.query.isNotEmpty()) {
                            IconButton(onClick = { vm.onQuery("") }) { Icon(Icons.Default.Clear, contentDescription = stringResource(R.string.clear_search)) }
                        }
                    },
                    singleLine = true,
                    modifier = Modifier.fillMaxWidth(),
                )
            }
            item {
                ChipRow(horizontalPadding = 0.dp) {
                    ClientFilter.entries.forEach { f ->
                        val count = (ui.clients as? Loadable.Ready)?.value?.counts?.let {
                            when (f) {
                                ClientFilter.Flats -> it.flats
                                ClientFilter.Unregistered -> it.unregistered
                                ClientFilter.Online -> it.online
                                ClientFilter.All -> null
                            }
                        }
                        val name = stringResource(f.labelRes)
                        FilterChip(
                            selected = ui.filter == f,
                            onClick = { vm.setFilter(f) },
                            label = { ChipLabel(if (count != null) "$name $count" else name) },
                        )
                    }
                }
            }
            when (val c = ui.clients) {
                Loadable.Loading -> item { PaneMessage { LoadingSpinner() } }
                is Loadable.Failed -> item {
                    PaneMessage {
                        Text(c.message, style = MaterialTheme.typography.bodyLarge, textAlign = TextAlign.Center)
                        Button(onClick = vm::retry) { Text(stringResource(R.string.action_try_again)) }
                    }
                }
                is Loadable.Ready -> if (c.value.items.isEmpty()) {
                    item {
                        PaneMessage {
                            Text(
                                stringResource(if (ui.query.isBlank() && ui.filter == ClientFilter.All) R.string.no_devices_yet else R.string.nothing_matches),
                                color = MaterialTheme.colorScheme.onSurfaceVariant,
                                textAlign = TextAlign.Center,
                            )
                        }
                    }
                } else {
                    items(c.value.items, key = { it.key }) { cl ->
                        ClientRow(
                            cl, expanded = ui.expanded == cl.key, busy = ui.busy == cl.key, canWrite = user.canWrite,
                            onToggle = { vm.toggle(cl.key) },
                            onOpen = { onOpenCustomer(cl.accountId) },
                            onChangeMac = { onChangeMac(cl.accountId) },
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
    PullToRefreshBox(ui.refreshing, vm::refresh, Modifier.fillMaxSize()) {
        LazyColumn(
            Modifier.fillMaxSize(),
            contentPadding = PaddingValues(start = 16.dp, end = 16.dp, top = 0.dp, bottom = 24.dp),
            verticalArrangement = Arrangement.spacedBy(8.dp),
        ) {
            item { TabChips(ui, vm) }
            when (val l = ui.interfaces) {
                Loadable.Loading -> item { PaneMessage { LoadingSpinner() } }
                is Loadable.Failed -> item {
                    PaneMessage {
                        Text(l.message, style = MaterialTheme.typography.bodyLarge, textAlign = TextAlign.Center)
                        Button(onClick = vm::retry) { Text(stringResource(R.string.action_try_again)) }
                    }
                }
                is Loadable.Ready -> if (l.value.isEmpty()) {
                    item { PaneMessage { Text(stringResource(R.string.no_interfaces), color = MaterialTheme.colorScheme.onSurfaceVariant) } }
                } else {
                    items(l.value, key = { it.name }) { i ->
                        InterfaceRow(i, expanded = ui.expanded == "i-${i.name}", onToggle = { vm.toggle("i-${i.name}") })
                    }
                }
            }
        }
    }
}
