package ke.co.ftw.ftnadmin.ui.customers

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.KeyboardArrowRight
import androidx.compose.material.icons.filled.Add
import androidx.compose.material.icons.filled.Clear
import androidx.compose.material.icons.filled.Search
import androidx.compose.material3.AssistChip
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FilterChip
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.ExtendedFloatingActionButton
import androidx.compose.material3.Text
import androidx.compose.material3.pulltorefresh.PullToRefreshBox
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.snapshotFlow
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import ke.co.ftw.ftnadmin.R
import ke.co.ftw.ftnadmin.domain.Account
import ke.co.ftw.ftnadmin.ui.common.MonoText
import ke.co.ftw.ftnadmin.ui.common.formatPhone
import ke.co.ftw.ftnadmin.core.network.UserInfo
import ke.co.ftw.ftnadmin.ui.common.TabScaffold
import ke.co.ftw.ftnadmin.ui.common.ChipLabel
import ke.co.ftw.ftnadmin.ui.common.ChipRow
import ke.co.ftw.ftnadmin.ui.common.LoadingSpinner
import ke.co.ftw.ftnadmin.ui.common.LocalContainer
import ke.co.ftw.ftnadmin.ui.common.StatusChip
import ke.co.ftw.ftnadmin.ui.common.Tone
import ke.co.ftw.ftnadmin.ui.common.formatExpiry
import ke.co.ftw.ftnadmin.ui.common.vmFactory

fun accountTone(a: Account): Tone = when {
    a.macConflict -> Tone.Bad
    a.suspended || a.state == "suspended" -> Tone.Bad
    a.state == "active" -> Tone.Good
    a.state == "expired" -> Tone.Warn
    else -> Tone.Neutral
}

fun accountStateLabel(a: Account): String = when {
    a.macConflict -> "MAC conflict"
    a.suspended -> "Suspended"
    else -> a.state.replaceFirstChar { it.uppercase() }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun CustomersScreen(user: UserInfo, onAlerts: () -> Unit, onOpen: (String) -> Unit, onAdd: () -> Unit, onApartments: () -> Unit = {}, onEquipment: () -> Unit = {}) {
    val container = LocalContainer.current
    val vm: CustomersViewModel = viewModel(
        factory = vmFactory { CustomersViewModel(container.accounts, selection = container.routerContext.selection) },
    )
    val ui by vm.state.collectAsStateWithLifecycle()
    val listState = rememberLazyListState()

    LaunchedEffect(listState, ui.items.size) {
        snapshotFlow { listState.layoutInfo.visibleItemsInfo.lastOrNull()?.index ?: 0 }.collect { last ->
            if (last >= ui.items.size - 5) vm.loadMore()
        }
    }

    TabScaffold(
        title = stringResource(R.string.tab_customers),
        onAlerts = onAlerts,
        floatingActionButton = {
            if (user.canWrite) {
                ExtendedFloatingActionButton(
                    onClick = onAdd,
                    icon = { Icon(Icons.Default.Add, contentDescription = null) },
                    text = { Text(stringResource(R.string.add_customer), maxLines = 1, softWrap = false) },
                    containerColor = MaterialTheme.colorScheme.primaryContainer,
                    contentColor = MaterialTheme.colorScheme.onPrimaryContainer,
                )
            }
        },
    ) { inner ->
        // One scrolling list: search and chips are its first rows, so large fonts never squeeze the customers out of view.
        PullToRefreshBox(isRefreshing = ui.refreshing, onRefresh = vm::refresh, modifier = Modifier.padding(inner).fillMaxSize()) {
            LazyColumn(
                state = listState,
                modifier = Modifier.fillMaxSize(),
                contentPadding = androidx.compose.foundation.layout.PaddingValues(start = 16.dp, end = 16.dp, top = 8.dp, bottom = 96.dp),
                verticalArrangement = Arrangement.spacedBy(8.dp),
            ) {
                item {
                    OutlinedTextField(
                        value = ui.query,
                        onValueChange = vm::onQuery,
                        placeholder = { Text(stringResource(R.string.search_customers), maxLines = 1, overflow = TextOverflow.Ellipsis) },
                        supportingText = { Text(stringResource(R.string.search_customers_hint)) },
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
                    // Places to go, apart from the filters below: assist chips with a chevron, not filter chips.
                    ChipRow(horizontalPadding = 0.dp) {
                        AssistChip(onClick = onApartments, label = { ChipLabel(stringResource(R.string.apartments)) }, trailingIcon = { Icon(Icons.AutoMirrored.Filled.KeyboardArrowRight, contentDescription = null) })
                        AssistChip(onClick = onEquipment, label = { ChipLabel(stringResource(R.string.equipment)) }, trailingIcon = { Icon(Icons.AutoMirrored.Filled.KeyboardArrowRight, contentDescription = null) })
                    }
                }
                item {
                    ChipRow(horizontalPadding = 0.dp) {
                        FilterChip(selected = ui.stateFilter == null, onClick = { vm.onStateFilter(null) }, label = { ChipLabel(stringResource(R.string.filter_all)) })
                        ACCOUNT_STATES.forEach { st ->
                            FilterChip(
                                selected = ui.stateFilter == st,
                                onClick = { vm.onStateFilter(if (ui.stateFilter == st) null else st) },
                                label = { ChipLabel(st.replaceFirstChar { it.uppercase() }) },
                            )
                        }
                    }
                }
                when {
                    ui.loading -> item { Box(Modifier.fillMaxWidth().padding(32.dp), contentAlignment = Alignment.Center) { LoadingSpinner() } }
                    ui.error != null && ui.items.isEmpty() -> item {
                        Column(Modifier.fillMaxWidth().padding(24.dp), horizontalAlignment = Alignment.CenterHorizontally, verticalArrangement = Arrangement.spacedBy(12.dp)) {
                            Text(ui.error!!, style = MaterialTheme.typography.bodyLarge, textAlign = TextAlign.Center)
                            Button(onClick = vm::retry) { Text(stringResource(R.string.action_try_again)) }
                        }
                    }
                    ui.items.isEmpty() -> item {
                        Text(
                            stringResource(if (ui.query.isBlank() && ui.stateFilter == null) R.string.no_customers_yet else R.string.no_customers_match),
                            style = MaterialTheme.typography.bodyLarge,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                            textAlign = TextAlign.Center,
                            modifier = Modifier.fillMaxWidth().padding(24.dp),
                        )
                    }
                    else -> {
                        items(ui.items, key = { it.id }) { a -> AccountRow(a) { onOpen(a.id) } }
                        if (ui.loadingMore) {
                            item { Row(Modifier.fillMaxWidth().padding(8.dp), horizontalArrangement = Arrangement.Center) { LoadingSpinner() } }
                        }
                        ui.error?.let { e -> item { Text(e, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodyMedium) } }
                    }
                }
            }
        }
    }
}

@Composable
fun AccountRow(a: Account, onClick: () -> Unit) {
    Card(
        onClick = onClick,
        modifier = Modifier.fillMaxWidth(),
        colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceContainer),
    ) {
        Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
            Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.spacedBy(8.dp), verticalAlignment = Alignment.Top) {
                Text(a.headline, style = MaterialTheme.typography.titleMedium, color = MaterialTheme.colorScheme.onSurface, maxLines = 2, overflow = TextOverflow.Ellipsis, modifier = Modifier.weight(1f))
                StatusChip(accountStateLabel(a), accountTone(a), Modifier.weight(1f, fill = false))
            }
            if (a.placeLine.isNotBlank()) Text(a.placeLine, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            if (a.phone.isNotBlank()) MonoText(formatPhone(a.phone), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            Text(
                listOfNotNull(a.routerLabel.takeIf { it.isNotBlank() }, a.pkg?.name, if (a.expiresAt != null) formatExpiry(a.expiresAt) else null).joinToString("  |  "),
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
    }
}
