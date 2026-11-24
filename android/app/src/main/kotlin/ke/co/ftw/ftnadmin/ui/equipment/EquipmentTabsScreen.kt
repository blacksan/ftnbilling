package ke.co.ftw.ftnadmin.ui.equipment

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Add
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.ExtendedFloatingActionButton
import androidx.compose.material3.FilterChip
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.PrimaryTabRow
import androidx.compose.material3.Scaffold
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Tab
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
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
import ke.co.ftw.ftnadmin.data.onu.RegisterItem
import ke.co.ftw.ftnadmin.ui.common.AppTopBar
import ke.co.ftw.ftnadmin.ui.common.CenteredLoading
import ke.co.ftw.ftnadmin.ui.common.ChipLabel
import ke.co.ftw.ftnadmin.ui.common.ChipRow
import ke.co.ftw.ftnadmin.ui.common.EmptyState
import ke.co.ftw.ftnadmin.ui.common.ErrorState
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.common.LocalContainer
import ke.co.ftw.ftnadmin.ui.common.StatusChip
import ke.co.ftw.ftnadmin.ui.common.Tone
import ke.co.ftw.ftnadmin.ui.common.vmFactory
import ke.co.ftw.ftnadmin.ui.routers.RoutersTab
import ke.co.ftw.ftnadmin.ui.routers.RoutersViewModel

/**
 * Equipment: one screen, four tabs (Routers, OLT, ONU, Other). The lists reuse what already exists: the router list and
 * its view model, the equipment tree (and its "place an ONU" sheet) for OLT and ONU, the register for Other. The tab is
 * state of this one back-stack entry, so Back leaves Equipment from any tab; the tree is a screen of its own that Back
 * returns from. [initialTab] comes from the route, so a link can open a tab directly.
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun EquipmentTabsScreen(
    user: UserInfo,
    initialTab: EquipmentTab,
    onBack: () -> Unit,
    onOpenRouter: (String) -> Unit,
    onAddRouter: () -> Unit,
    onAddOlt: (String?) -> Unit,
    onEditOlt: (String) -> Unit,
    onAddOnu: () -> Unit,
    onOpenAccount: (String) -> Unit,
    onTree: () -> Unit,
) {
    val container = LocalContainer.current
    val tabsVm: EquipmentTabsViewModel = viewModel(key = "equipment-tabs", factory = vmFactory { EquipmentTabsViewModel(container.equipment, initialTab) })
    val routersVm: RoutersViewModel = viewModel(key = "equipment-routers", factory = vmFactory { RoutersViewModel(container.sites) })
    val treeVm: EquipmentViewModel = viewModel(key = "equipment-tabs-tree", factory = vmFactory { EquipmentViewModel(container.equipment) })
    val ui by tabsVm.state.collectAsStateWithLifecycle()
    val routers by routersVm.state.collectAsStateWithLifecycle()
    val treeUi by treeVm.state.collectAsStateWithLifecycle()
    val snack = remember { SnackbarHostState() }
    LaunchedEffect(treeUi.info) {
        treeUi.info?.let {
            snack.showSnackbar(it)
            treeVm.clearInfo()
        }
    }
    // Coming back from a form: what was added shows up.
    LaunchedEffect(Unit) {
        treeVm.load()
        tabsVm.loadRegister()
        routersVm.refresh()
    }
    val tree = (treeUi.tree as? Loadable.Ready)?.value
    val counts = equipmentTabCounts(
        routers = (routers.sites as? Loadable.Ready)?.value?.size,
        tree = tree,
        other = (ui.register as? Loadable.Ready)?.value,
    )
    Scaffold(
        containerColor = MaterialTheme.colorScheme.background,
        topBar = {
            Column {
                AppTopBar("Equipment", onBack = onBack, actions = { TextButton(onClick = onTree) { Text("Tree") } })
                PrimaryTabRow(selectedTabIndex = ui.tab.ordinal) {
                    EquipmentTab.entries.forEach { t ->
                        Tab(
                            selected = ui.tab == t,
                            onClick = { tabsVm.selectTab(t) },
                            text = { Text(tabTitle(t, counts), maxLines = 1, softWrap = false) },
                        )
                    }
                }
            }
        },
        floatingActionButton = {
            val add: Pair<String, () -> Unit>? = when (ui.tab) {
                EquipmentTab.Routers -> if (user.isOwner) "Add router" to onAddRouter else null
                EquipmentTab.Olt -> if (user.canWrite) "Add OLT" to { onAddOlt(null) } else null
                EquipmentTab.Onu -> if (user.canWrite) "Add ONU" to onAddOnu else null
                EquipmentTab.Other -> null
            }
            if (add != null) {
                ExtendedFloatingActionButton(
                    onClick = add.second,
                    icon = { Icon(Icons.Default.Add, contentDescription = null) },
                    text = { Text(add.first, fontWeight = FontWeight.ExtraBold, maxLines = 1, softWrap = false) },
                    containerColor = MaterialTheme.colorScheme.primaryContainer,
                    contentColor = MaterialTheme.colorScheme.onPrimaryContainer,
                )
            }
        },
        snackbarHost = { SnackbarHost(snack) },
    ) { inner ->
        val body = Modifier.padding(inner)
        when (ui.tab) {
            EquipmentTab.Routers -> RoutersTab(routersVm, onOpenRouter, body)
            EquipmentTab.Olt -> TreeBacked(treeUi.tree, treeVm::load, body) { t ->
                val rows = t.oltRows()
                if (rows.isEmpty()) {
                    EmptyState("No OLTs yet. An OLT is the box the ONUs hang off.")
                } else {
                    LazyColumn(Modifier.fillMaxSize(), contentPadding = LIST_PADDING, verticalArrangement = Arrangement.spacedBy(10.dp)) {
                        items(rows, key = { it.olt.id }) { r -> OltTabRow(r) { onEditOlt(r.olt.id) } }
                    }
                }
            }
            EquipmentTab.Onu -> TreeBacked(treeUi.tree, treeVm::load, body) { t ->
                val all = t.onuRows()
                val rows = filterOnuRows(all, ui.onuQuery, ui.onuStatus, ui.onuOltId)
                LazyColumn(Modifier.fillMaxSize(), contentPadding = LIST_PADDING, verticalArrangement = Arrangement.spacedBy(10.dp)) {
                    item {
                        OutlinedTextField(
                            ui.onuQuery, tabsVm::setOnuQuery, label = { Text("Serial, MAC, model or house") }, singleLine = true,
                            modifier = Modifier.fillMaxWidth(),
                        )
                    }
                    item {
                        ChipRow(horizontalPadding = 0.dp) {
                            ONU_STATUSES.forEach { s -> FilterChip(ui.onuStatus == s, { tabsVm.toggleOnuStatus(s) }, label = { ChipLabel(onuStatusLabel(s)) }) }
                        }
                    }
                    if (treeUi.allOlts.isNotEmpty()) {
                        item {
                            ChipRow(horizontalPadding = 0.dp) {
                                treeUi.allOlts.forEach { o -> FilterChip(ui.onuOltId == o.id, { tabsVm.toggleOnuOlt(o.id) }, label = { ChipLabel(o.name) }) }
                            }
                        }
                    }
                    if (rows.isEmpty()) item { Text(if (all.isEmpty()) "No ONUs yet. Add one to the register." else "No ONU matches.", color = MaterialTheme.colorScheme.onSurfaceVariant) }
                    items(rows, key = { it.onu.id }) { r ->
                        OnuNode(r.onu, user.canWrite, treeVm::startMove, onOpenAccount, placement = onuPlacement(r))
                    }
                }
            }
            EquipmentTab.Other -> when (val o = ui.other) {
                Loadable.Loading -> CenteredLoading(body)
                is Loadable.Failed -> ErrorState(o.message, tabsVm::loadRegister, body)
                is Loadable.Ready -> {
                    val rows = filterOther(o.value, ui.otherQuery)
                    LazyColumn(Modifier.padding(inner).fillMaxSize(), contentPadding = LIST_PADDING, verticalArrangement = Arrangement.spacedBy(10.dp)) {
                        item {
                            OutlinedTextField(
                                ui.otherQuery, tabsVm::setOtherQuery, label = { Text("Serial, MAC or model") }, singleLine = true,
                                modifier = Modifier.fillMaxWidth(),
                            )
                        }
                        if (rows.isEmpty()) {
                            item { Text(if (o.value.isEmpty()) "No other equipment. Switches, access points, splitters and UPS units are listed here." else "Nothing matches.", color = MaterialTheme.colorScheme.onSurfaceVariant) }
                        }
                        items(rows, key = { it.id }) { OtherRow(it) }
                    }
                }
            }
        }
    }
    treeUi.move?.let { m -> MoveSheetContent(m, treeUi.allOlts, treeVm) }
}

private val LIST_PADDING = PaddingValues(start = 16.dp, end = 16.dp, top = 12.dp, bottom = 96.dp)

/** Where an ONU hangs: "Router, OLT, PON 2", or "Not placed". */
fun onuPlacement(r: OnuRow): String {
    if (r.oltName.isBlank()) return "Not placed on an OLT"
    return listOf(r.routerName, r.oltName, if (r.onu.ponPort > 0) "PON ${r.onu.ponPort}" else "").filter { it.isNotBlank() }.joinToString("  |  ")
}

@Composable
private fun TreeBacked(
    tree: Loadable<ke.co.ftw.ftnadmin.data.equipment.EquipmentTree>,
    retry: () -> Unit,
    modifier: Modifier,
    content: @Composable (ke.co.ftw.ftnadmin.data.equipment.EquipmentTree) -> Unit,
) {
    when (tree) {
        Loadable.Loading -> CenteredLoading(modifier)
        is Loadable.Failed -> ErrorState(tree.message, retry, modifier)
        is Loadable.Ready -> Column(modifier.fillMaxSize()) { content(tree.value) }
    }
}

@OptIn(ExperimentalLayoutApi::class)
@Composable
private fun OltTabRow(r: OltRow, onClick: () -> Unit) {
    val o = r.olt
    Card(onClick = onClick, modifier = Modifier.fillMaxWidth(), colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceContainer)) {
        Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
            Text(o.name, style = MaterialTheme.typography.titleMedium, color = MaterialTheme.colorScheme.onSurface)
            Text(
                listOf(o.model, o.mac).filter { it.isNotBlank() }.joinToString("  |  "),
                style = MaterialTheme.typography.bodyMedium,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            Text(
                (if (r.routerName.isBlank()) "Not on a router" else r.routerName + if (o.routerPort.isNotBlank()) ", port ${o.routerPort}" else "") +
                    "  |  ${o.ponPorts} PON ports",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                StatusChip("${o.onlineCount} of ${o.onuCount} ONUs online", if (o.warningCount > 0) Tone.Warn else Tone.Good)
                if (o.status.isNotBlank()) StatusChip(onuStatusLabel(o.status), Tone.Neutral)
            }
        }
    }
}

@Composable
private fun OtherRow(i: RegisterItem) {
    Card(Modifier.fillMaxWidth(), colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceContainer)) {
        Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
            Text(i.serial.ifBlank { i.mac.ifBlank { "(no serial)" } }, style = MaterialTheme.typography.titleMedium, color = MaterialTheme.colorScheme.onSurface)
            Text(
                listOf(i.kind, i.model, i.mac).filter { it.isNotBlank() }.joinToString("  |  "),
                style = MaterialTheme.typography.bodyMedium,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            if (i.status.isNotBlank()) StatusChip(onuStatusLabel(i.status), Tone.Neutral)
        }
    }
}
