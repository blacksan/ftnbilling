package ke.co.ftw.ftnadmin.ui.equipment

import androidx.activity.compose.BackHandler
import androidx.compose.foundation.layout.imePadding
import androidx.compose.ui.res.pluralStringResource
import androidx.compose.ui.res.stringResource
import ke.co.ftw.ftnadmin.R
import ke.co.ftw.ftnadmin.ui.common.MonoText
import ke.co.ftw.ftnadmin.ui.common.houseText
import ke.co.ftw.ftnadmin.ui.common.onlineStateRes
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
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FilterChip
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.ModalBottomSheet
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.rememberModalBottomSheetState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import ke.co.ftw.ftnadmin.core.network.UserInfo
import ke.co.ftw.ftnadmin.data.equipment.TreeOlt
import ke.co.ftw.ftnadmin.data.equipment.TreeOnu
import ke.co.ftw.ftnadmin.data.equipment.TreePon
import ke.co.ftw.ftnadmin.ui.common.AppTopBar
import ke.co.ftw.ftnadmin.ui.common.CenteredLoading
import ke.co.ftw.ftnadmin.ui.common.ChipLabel
import ke.co.ftw.ftnadmin.ui.common.ChipRow
import ke.co.ftw.ftnadmin.ui.common.EmptyState
import ke.co.ftw.ftnadmin.ui.common.ErrorState
import ke.co.ftw.ftnadmin.ui.common.LabeledValue
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.common.LocalContainer
import ke.co.ftw.ftnadmin.ui.common.StatusChip
import ke.co.ftw.ftnadmin.ui.common.Tone
import ke.co.ftw.ftnadmin.ui.common.vmFactory

/**
 * Equipment: router > OLT > PON port > ONU > house, one level per step. Back climbs a level; at the top it leaves.
 * A router's apartments (the logical side) sit next to its OLTs (the physical side).
 */
@OptIn(ExperimentalLayoutApi::class, ExperimentalMaterial3Api::class)
@Composable
fun EquipmentScreen(
    user: UserInfo,
    onBack: () -> Unit,
    onOpenAccount: (String) -> Unit,
    onAddOlt: (String?) -> Unit,
    onEditOlt: (String) -> Unit,
) {
    val container = LocalContainer.current
    val vm: EquipmentViewModel = viewModel(key = "equipment", factory = vmFactory { EquipmentViewModel(container.equipment) })
    val ui by vm.state.collectAsStateWithLifecycle()
    val snack = remember { SnackbarHostState() }
    LaunchedEffect(ui.info) {
        ui.info?.let {
            snack.showSnackbar(it)
            vm.clearInfo()
        }
    }
    // Back from the OLT form: the tree may have changed.
    LaunchedEffect(Unit) { vm.load() }
    BackHandler(enabled = ui.level != EquipmentLevel.Root) { vm.up() }
    Scaffold(
        containerColor = MaterialTheme.colorScheme.background,
        topBar = { AppTopBar(ui.title, onBack = { if (!vm.up()) onBack() }) },
        snackbarHost = { SnackbarHost(snack) },
    ) { inner ->
        when (val t = ui.tree) {
            Loadable.Loading -> CenteredLoading(Modifier.padding(inner))
            is Loadable.Failed -> ErrorState(t.message, vm::load, Modifier.padding(inner))
            is Loadable.Ready -> LazyColumn(
                Modifier.padding(inner).fillMaxSize(),
                contentPadding = PaddingValues(16.dp),
                verticalArrangement = Arrangement.spacedBy(12.dp),
            ) {
                when (ui.level) {
                    EquipmentLevel.Root -> {
                        if (user.canWrite) item { Button(onClick = { onAddOlt(null) }, modifier = Modifier.fillMaxWidth()) { Text(stringResource(R.string.add_an_olt)) } }
                        if (t.value.routers.isEmpty() && t.value.unplacedOlts.isEmpty() && t.value.unplacedOnus.isEmpty()) {
                            item { EmptyState(stringResource(R.string.no_equipment_yet)) }
                        }
                        items(t.value.routers, key = { it.id }) { r ->
                            Node(
                                r.name,
                                stringResource(
                                    R.string.router_counts,
                                    pluralStringResource(R.plurals.olt_count, r.olts.size, r.olts.size),
                                    pluralStringResource(R.plurals.onu_count, r.onuCount, r.onuCount),
                                    r.onlineCount,
                                ) +
                                    if (r.apartments.isNotEmpty()) "  |  " + r.apartments.joinToString(", ") { it.name } else "",
                            ) { vm.openRouter(r.id) }
                        }
                        if (t.value.unplacedOlts.isNotEmpty() || t.value.unplacedOnus.isNotEmpty()) {
                            item {
                                Node(
                                    stringResource(R.string.not_placed),
                                    stringResource(
                                        R.string.not_placed_counts,
                                        pluralStringResource(R.plurals.olt_count, t.value.unplacedOlts.size, t.value.unplacedOlts.size),
                                        pluralStringResource(R.plurals.onu_count, t.value.unplacedOnus.size, t.value.unplacedOnus.size),
                                    ),
                                ) { vm.openRouter(UNPLACED) }
                            }
                        }
                    }
                    EquipmentLevel.Router -> {
                        if (user.canWrite) item { Button(onClick = { onAddOlt(ui.routerId.takeIf { it != UNPLACED }) }, modifier = Modifier.fillMaxWidth()) { Text(stringResource(R.string.add_an_olt_here)) } }
                        ui.router?.let { r ->
                            if (r.apartments.isNotEmpty()) {
                                item {
                                    Text(
                                        stringResource(R.string.apartments_list, r.apartments.joinToString(", ") { it.name }),
                                        style = MaterialTheme.typography.bodyMedium,
                                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                                    )
                                }
                            }
                        }
                        if (ui.olts.isEmpty()) item { EmptyState(stringResource(R.string.no_olts_on_router)) }
                        items(ui.olts, key = { it.id }) { o -> OltNode(o) { vm.openOlt(o.id) } }
                        if (ui.routerId == UNPLACED) {
                            items(ui.unplacedOnus, key = { "u" + it.id }) { onu -> OnuNode(onu, user.canWrite, vm::startMove, onOpenAccount) }
                        }
                    }
                    EquipmentLevel.Olt -> ui.olt?.let { o ->
                        item { OltDetail(o, user.canWrite) { onEditOlt(o.id) } }
                        if (o.pons.isEmpty()) item { EmptyState(stringResource(R.string.olt_no_pon_ports)) }
                        items(o.pons, key = { it.port }) { p -> PonNode(p) { vm.openPon(p.port) } }
                    }
                    EquipmentLevel.Pon -> ui.pon?.let { p ->
                        item {
                            Text(
                                stringResource(R.string.serves, p.apartmentText),
                                style = MaterialTheme.typography.bodyMedium,
                                color = MaterialTheme.colorScheme.onSurfaceVariant,
                            )
                        }
                        if (p.onus.isEmpty()) item { EmptyState(stringResource(R.string.no_onus_on_port)) }
                        items(p.onus, key = { it.id }) { onu -> OnuNode(onu, user.canWrite, vm::startMove, onOpenAccount) }
                    }
                }
            }
        }
    }
    ui.move?.let { m -> MoveSheetContent(m, ui.allOlts, vm) }
}

@Composable
private fun Node(title: String, subtitle: String, onClick: () -> Unit) {
    Card(onClick = onClick, modifier = Modifier.fillMaxWidth(), colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceContainer)) {
        Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
            Text(title, style = MaterialTheme.typography.titleMedium, color = MaterialTheme.colorScheme.onSurface)
            Text(subtitle, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
    }
}

@OptIn(ExperimentalLayoutApi::class)
@Composable
internal fun OltNode(o: TreeOlt, onClick: () -> Unit) {
    Card(onClick = onClick, modifier = Modifier.fillMaxWidth(), colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceContainer)) {
        Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
            Text(o.name, style = MaterialTheme.typography.titleMedium, color = MaterialTheme.colorScheme.onSurface)
            MonoText(o.mac, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
            FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                StatusChip("OLT", Tone.Neutral)
                StatusChip(o.summary, if (o.warningCount > 0) Tone.Warn else Tone.Good)
            }
        }
    }
}

@Composable
private fun OltDetail(o: TreeOlt, canWrite: Boolean, onEdit: () -> Unit) {
    Card(Modifier.fillMaxWidth(), colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceContainer)) {
        Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
            LabeledValue(stringResource(R.string.mac_address), o.mac, mono = true)
            LabeledValue(stringResource(R.string.model), o.model)
            LabeledValue(stringResource(R.string.serial), o.serial, mono = true)
            LabeledValue("Firmware", o.firmware)
            LabeledValue(stringResource(R.string.router_port), o.routerPort, mono = true)
            LabeledValue("PON ports", if (o.ponPorts > 0) o.ponPorts.toString() else "")
            LabeledValue(stringResource(R.string.management), listOf(o.mgmtIp, o.mgmtUser).filter { it.isNotBlank() }.joinToString("  |  "), mono = true)
            // Only whether a password is stored: the value is never sent to the app.
            LabeledValue(stringResource(R.string.password), stringResource(if (o.hasMgmtPass) R.string.stored else R.string.not_stored))
            LabeledValue(stringResource(R.string.onus), o.summary)
            if (canWrite) OutlinedButton(onClick = onEdit, modifier = Modifier.fillMaxWidth()) { Text(stringResource(R.string.edit_olt)) }
        }
    }
}

@Composable
private fun PonNode(p: TreePon, onClick: () -> Unit) {
    Node(stringResource(R.string.pon_port_n, p.port), "${pluralStringResource(R.plurals.onu_count, p.onus.size, p.onus.size)}  |  ${p.apartmentText}", onClick)
}

@OptIn(ExperimentalLayoutApi::class)
@Composable
internal fun OnuNode(onu: TreeOnu, canWrite: Boolean, onMove: (TreeOnu) -> Unit, onOpenAccount: (String) -> Unit, placement: String = "") {
    Card(Modifier.fillMaxWidth(), colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceContainer)) {
        Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(6.dp)) {
            Text(onu.title, style = MaterialTheme.typography.titleMedium, color = MaterialTheme.colorScheme.onSurface)
            if (onu.model.isNotBlank()) Text(onu.model, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
            if (onu.mac.isNotBlank()) MonoText(onu.mac, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
            if (placement.isNotBlank()) Text(placement, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                if (onu.status.isNotBlank()) StatusChip(onuStatusLabel(onu.status), Tone.Neutral)
                StatusChip(stringResource(onlineStateRes(onu.online)), if (onu.online) Tone.Good else Tone.Neutral)
                onu.tr069?.let { StatusChip(stringResource(if (it.heldBack) R.string.tr069_held_back else R.string.tr069), if (it.heldBack) Tone.Warn else Tone.Neutral) }
            }
            onu.tr069?.lastInform?.takeIf { it.isNotBlank() }?.let {
                Text(stringResource(R.string.tr069_last_checkin, it.take(16).replace('T', ' ')), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            }
            val h = onu.house
            if (h != null) {
                // A text button: 48 dp tall, announced as a button, not a coloured line of text.
                TextButton(onClick = { onOpenAccount(h.accountId) }) {
                    Text(houseText(h.unit) + if (h.apartment.isNotBlank()) ", ${h.apartment}" else "")
                }
            } else {
                Text(stringResource(R.string.not_linked_to_house), style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
            }
            onu.warnings.forEach { Text(it, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.error) }
            if (canWrite) TextButton(onClick = { onMove(onu) }) { Text(stringResource(if (onu.oltId.isBlank()) R.string.place_on_olt else R.string.action_move)) }
        }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
internal fun MoveSheetContent(m: MoveSheet, olts: List<TreeOlt>, vm: EquipmentViewModel) {
    ModalBottomSheet(onDismissRequest = vm::cancelMove, sheetState = rememberModalBottomSheetState(skipPartiallyExpanded = true)) {
        Column(
            Modifier.fillMaxWidth().imePadding().verticalScroll(rememberScrollState()).padding(horizontal = 16.dp).padding(top = 8.dp, bottom = 16.dp),
            verticalArrangement = Arrangement.spacedBy(10.dp),
        ) {
            Text(stringResource(R.string.place_title, m.onu.title), style = MaterialTheme.typography.titleLarge, color = MaterialTheme.colorScheme.onSurface)
            Text("OLT", style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
            FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                olts.forEach { o -> FilterChip(m.oltId == o.id, { vm.setMoveOlt(o.id) }, label = { Text(o.name) }) }
            }
            if (m.oltId.isNotBlank()) {
                OutlinedTextField(
                    m.port, vm::setMovePort, label = { Text(stringResource(R.string.pon_port)) }, singleLine = true,
                    keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Number), modifier = Modifier.fillMaxWidth(),
                )
            }
            m.error?.let { Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodyMedium) }
            Button(onClick = vm::applyMove, enabled = !m.busy, modifier = Modifier.fillMaxWidth()) {
                Text(stringResource(if (m.busy) R.string.saving else if (m.oltId.isBlank()) R.string.take_out_of_olt else R.string.action_save))
            }
            TextButton(onClick = vm::cancelMove, modifier = Modifier.fillMaxWidth()) { Text(stringResource(R.string.action_cancel)) }
        }
    }
}
