package ke.co.ftw.ftnadmin.ui.customers

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Clear
import androidx.compose.material.icons.filled.Search
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FilterChip
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.pulltorefresh.PullToRefreshBox
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.snapshotFlow
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import ke.co.ftw.ftnadmin.domain.Account
import ke.co.ftw.ftnadmin.ui.common.AppTopBar
import ke.co.ftw.ftnadmin.ui.common.CenteredLoading
import ke.co.ftw.ftnadmin.ui.common.ChipLabel
import ke.co.ftw.ftnadmin.ui.common.ChipRow
import ke.co.ftw.ftnadmin.ui.common.EmptyState
import ke.co.ftw.ftnadmin.ui.common.ErrorState
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
fun CustomersScreen(onSettings: () -> Unit, onOpen: (String) -> Unit) {
    val container = LocalContainer.current
    val vm: CustomersViewModel = viewModel(factory = vmFactory { CustomersViewModel(container.accounts) })
    val ui by vm.state.collectAsStateWithLifecycle()
    val listState = rememberLazyListState()

    LaunchedEffect(listState, ui.items.size) {
        snapshotFlow { listState.layoutInfo.visibleItemsInfo.lastOrNull()?.index ?: 0 }.collect { last ->
            if (last >= ui.items.size - 5) vm.loadMore()
        }
    }

    Scaffold(
        containerColor = MaterialTheme.colorScheme.background,
        topBar = { AppTopBar("Customers", onSettings = onSettings) },
    ) { inner ->
        Column(Modifier.padding(inner).fillMaxSize()) {
            OutlinedTextField(
                value = ui.query,
                onValueChange = vm::onQuery,
                placeholder = { Text("Search flat, name, phone, MAC or code") },
                leadingIcon = { Icon(Icons.Default.Search, contentDescription = null) },
                trailingIcon = {
                    if (ui.query.isNotEmpty()) {
                        IconButton(onClick = { vm.onQuery("") }) { Icon(Icons.Default.Clear, contentDescription = "Clear search") }
                    }
                },
                singleLine = true,
                modifier = Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 8.dp),
            )
            ChipRow {
                FilterChip(selected = ui.stateFilter == null, onClick = { vm.onStateFilter(null) }, label = { ChipLabel("All") })
                ACCOUNT_STATES.forEach { st ->
                    FilterChip(
                        selected = ui.stateFilter == st,
                        onClick = { vm.onStateFilter(if (ui.stateFilter == st) null else st) },
                        label = { ChipLabel(st.replaceFirstChar { it.uppercase() }) },
                    )
                }
            }
            when {
                ui.loading -> CenteredLoading()
                ui.error != null && ui.items.isEmpty() -> ErrorState(ui.error!!, vm::retry)
                else -> PullToRefreshBox(isRefreshing = ui.refreshing, onRefresh = vm::refresh, modifier = Modifier.fillMaxSize()) {
                    if (ui.items.isEmpty()) {
                        EmptyState(if (ui.query.isBlank() && ui.stateFilter == null) "No customers yet." else "No customers match.")
                    } else {
                        LazyColumn(
                            state = listState,
                            modifier = Modifier.fillMaxSize(),
                            contentPadding = androidx.compose.foundation.layout.PaddingValues(16.dp),
                            verticalArrangement = Arrangement.spacedBy(8.dp),
                        ) {
                            items(ui.items, key = { it.id }) { a -> AccountRow(a) { onOpen(a.id) } }
                            if (ui.loadingMore) {
                                item { Row(Modifier.fillMaxWidth().padding(8.dp), horizontalArrangement = Arrangement.Center) { LoadingSpinner() } }
                            }
                            ui.error?.let { e -> item { Text(e, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall) } }
                        }
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
            Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.SpaceBetween) {
                Text("Flat ${a.flat}", style = MaterialTheme.typography.titleMedium, color = MaterialTheme.colorScheme.onSurface, maxLines = 1, overflow = androidx.compose.ui.text.style.TextOverflow.Ellipsis, modifier = Modifier.weight(1f).padding(end = 8.dp))
                StatusChip(accountStateLabel(a), accountTone(a))
            }
            val who = listOf(a.tenant, a.phone).filter { it.isNotBlank() }.joinToString(" - ")
            if (who.isNotBlank()) Text(who, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurface)
            Text(
                listOfNotNull(a.siteName.takeIf { it.isNotBlank() }, a.pkg?.name, if (a.expiresAt != null) formatExpiry(a.expiresAt) else null).joinToString("  |  "),
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
    }
}
