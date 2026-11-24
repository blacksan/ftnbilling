package ke.co.ftw.ftnadmin.ui.network

import androidx.compose.foundation.BorderStroke
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Place
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FilledTonalButton
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.ModalBottomSheet
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.pulltorefresh.PullToRefreshBox
import androidx.compose.material3.rememberModalBottomSheetState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import com.google.android.gms.maps.CameraUpdateFactory
import com.google.android.gms.maps.model.LatLng
import com.google.android.gms.maps.model.LatLngBounds
import com.google.maps.android.compose.GoogleMap
import com.google.maps.android.compose.MapUiSettings
import com.google.maps.android.compose.MarkerComposable
import com.google.maps.android.compose.rememberCameraPositionState
import com.google.maps.android.compose.rememberMarkerState
import ke.co.ftw.ftnadmin.BuildConfig
import ke.co.ftw.ftnadmin.data.networkmap.NetApartment
import ke.co.ftw.ftnadmin.ui.apartments.openMap
import ke.co.ftw.ftnadmin.ui.common.CenteredLoading
import ke.co.ftw.ftnadmin.ui.common.EmptyState
import ke.co.ftw.ftnadmin.ui.common.ErrorState
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.common.StatusChip

/** Where a tap on the sheet's rows goes: the existing screens. */
data class NetworkLinks(
    val onApartment: (String) -> Unit,
    val onRouter: (String) -> Unit,
    val onOlt: (String) -> Unit,
    /** An ONU row: its customer when the ONU is installed in a house, else the ONU tab of Equipment. */
    val onOnu: (accountId: String) -> Unit,
    val onOther: () -> Unit,
    val onAddGps: (String) -> Unit,
)

/** The Map and List views of the Network screen. [Live] stays in NetworkScreen. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun NetworkMapPane(ui: NetworkMapUiState, vm: NetworkMapViewModel, canWrite: Boolean, links: NetworkLinks) {
    when (val d = ui.data) {
        Loadable.Loading -> CenteredLoading()
        is Loadable.Failed -> ErrorState(d.message, vm::retry)
        is Loadable.Ready -> when {
            d.value.apartments.isEmpty() -> EmptyState("No apartments yet. Add an apartment with its GPS point and it appears here.")
            // The map is not inside pull-to-refresh: dragging the map down must pan it, not reload.
            ui.mode == NetworkMode.MapView && BuildConfig.MAPS_KEY_SET && ui.located.isNotEmpty() ->
                Column(Modifier.fillMaxSize()) {
                    Box(Modifier.weight(1f).fillMaxWidth()) { MapView(ui.located, onSelect = vm::select, onBackground = vm::dismiss) }
                    if (ui.unlocated.isNotEmpty()) NoLocationPanel(ui.unlocated, canWrite, links, Modifier.fillMaxWidth())
                }
            else -> PullToRefreshBox(ui.refreshing, vm::refresh, Modifier.fillMaxSize()) {
                ApartmentList(ui, vm, canWrite, links, showKeyNote = ui.mode == NetworkMode.MapView)
            }
        }
    }
    ui.selected?.let { a -> ApartmentSheet(a, canWrite, links, onDismiss = vm::dismiss) }
}

/** The Google map: one pin per apartment, teal / slate / red by [markerState]. A tap selects the apartment. */
@Composable
private fun MapView(located: List<NetApartment>, onSelect: (String) -> Unit, onBackground: () -> Unit) {
    val camera = rememberCameraPositionState()
    var loaded by remember { mutableStateOf(false) }
    val ids = located.map { it.id }
    LaunchedEffect(loaded, ids) {
        if (!loaded) return@LaunchedEffect
        val points = located.mapNotNull { a -> a.point?.let { LatLng(it.lat, it.lng) } }
        if (points.isEmpty()) return@LaunchedEffect
        camera.move(
            if (points.size == 1) {
                CameraUpdateFactory.newLatLngZoom(points[0], 16f)
            } else {
                val b = LatLngBounds.builder()
                points.forEach { b.include(it) }
                CameraUpdateFactory.newLatLngBounds(b.build(), 120)
            },
        )
    }
    GoogleMap(
        modifier = Modifier.fillMaxSize(),
        cameraPositionState = camera,
        uiSettings = MapUiSettings(zoomControlsEnabled = false, mapToolbarEnabled = false),
        onMapLoaded = { loaded = true },
        onMapClick = { onBackground() },
    ) {
        located.forEach { a ->
            val p = a.point ?: return@forEach
            val state = a.marker
            MarkerComposable(
                a.id, state,
                state = rememberMarkerState(key = a.id, position = LatLng(p.lat, p.lng)),
                title = a.name,
                onClick = {
                    onSelect(a.id)
                    true
                },
            ) { Pin(state) }
        }
    }
}

/** The pin: a coloured dot with a white ring and a white centre. The colour is never the only signal: the sheet says the state in words. */
@Composable
private fun Pin(state: MarkerState) {
    Box(
        Modifier.size(30.dp).background(Color(state.argb()), CircleShape).border(BorderStroke(3.dp, Color.White), CircleShape),
        contentAlignment = Alignment.Center,
    ) {
        Box(Modifier.size(8.dp).background(Color.White, CircleShape))
    }
}

/** The List view, and what the Map view shows when there is no Maps key or no apartment has a point yet. */
@Composable
private fun ApartmentList(ui: NetworkMapUiState, vm: NetworkMapViewModel, canWrite: Boolean, links: NetworkLinks, showKeyNote: Boolean) {
    val context = LocalContext.current
    val (down, partial, ok) = stateCounts(ui.apartments)
    LazyColumn(
        Modifier.fillMaxSize(),
        contentPadding = PaddingValues(start = 16.dp, end = 16.dp, top = 4.dp, bottom = 24.dp),
        verticalArrangement = Arrangement.spacedBy(8.dp),
    ) {
        if (showKeyNote) {
            item(key = "note") {
                Text(
                    if (!BuildConfig.MAPS_KEY_SET) {
                        "The map is off in this build: it has no Google Maps key. This is the same information as a list; tap Open in Google Maps on a row. How to add a key is in android/README.md."
                    } else {
                        "No apartment has a GPS point yet, so there is nothing to put on the map."
                    },
                    style = MaterialTheme.typography.bodyMedium,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
        }
        item(key = "legend") {
            Text(
                "Router down $down, some offline $partial, all good $ok",
                style = MaterialTheme.typography.labelLarge,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
        items(sortedForList(ui.apartments), key = { it.id }) { a ->
            Card(
                onClick = { vm.select(a.id) },
                modifier = Modifier.fillMaxWidth(),
                colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceContainerHigh),
            ) {
                Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(6.dp)) {
                    Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                        Text(a.name, style = MaterialTheme.typography.titleMedium, fontWeight = FontWeight.Bold, modifier = Modifier.weight(1f), maxLines = 2)
                        StatusChip(a.marker.label, a.marker.tone)
                    }
                    if (a.address.isNotBlank()) {
                        Text(a.address, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
                    }
                    Text(listSubtitle(a), style = MaterialTheme.typography.bodyMedium)
                    Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                        val point = a.point
                        if (point != null) {
                            OutlinedButton(onClick = { openMap(context, a.name, point) }) { Text("Open in Google Maps", maxLines = 1) }
                        } else if (canWrite) {
                            OutlinedButton(onClick = { links.onAddGps(a.id) }) { Text("Add GPS", maxLines = 1) }
                        } else {
                            Text("No location yet", style = MaterialTheme.typography.labelLarge, color = MaterialTheme.colorScheme.onSurfaceVariant)
                        }
                    }
                }
            }
        }
    }
}

/** Under the map: the apartments with no GPS point, each with a way to add one. Collapsed to one line until opened. */
@Composable
private fun NoLocationPanel(unlocated: List<NetApartment>, canWrite: Boolean, links: NetworkLinks, modifier: Modifier = Modifier) {
    var open by remember { mutableStateOf(false) }
    Card(modifier.padding(horizontal = 8.dp, vertical = 4.dp), colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceContainerHigh)) {
        Column(Modifier.padding(horizontal = 16.dp, vertical = 8.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Text("No location yet (${unlocated.size})", style = MaterialTheme.typography.titleSmall, fontWeight = FontWeight.Bold, modifier = Modifier.weight(1f))
                TextButton(onClick = { open = !open }) { Text(if (open) "Hide" else "Show") }
            }
            if (open) {
                Column(Modifier.verticalScroll(rememberScrollState()), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                    unlocated.forEach { a ->
                        Row(verticalAlignment = Alignment.CenterVertically) {
                            TextButton(onClick = { links.onApartment(a.id) }, modifier = Modifier.weight(1f)) {
                                Text(a.name, maxLines = 1, modifier = Modifier.fillMaxWidth())
                            }
                            if (canWrite) TextButton(onClick = { links.onAddGps(a.id) }) { Text("Add GPS", maxLines = 1) }
                        }
                    }
                }
            }
        }
    }
}

/** The bottom sheet of one apartment: the summary and the equipment installed there, each row opening its screen. */
@OptIn(ExperimentalMaterial3Api::class, ExperimentalLayoutApi::class)
@Composable
private fun ApartmentSheet(a: NetApartment, canWrite: Boolean, links: NetworkLinks, onDismiss: () -> Unit) {
    val context = LocalContext.current
    ModalBottomSheet(onDismissRequest = onDismiss, sheetState = rememberModalBottomSheetState()) {
        Column(
            Modifier.fillMaxWidth().verticalScroll(rememberScrollState()).padding(start = 16.dp, end = 16.dp, bottom = 24.dp),
            verticalArrangement = Arrangement.spacedBy(12.dp),
        ) {
            Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                Text(a.name, style = MaterialTheme.typography.titleLarge, fontWeight = FontWeight.ExtraBold, modifier = Modifier.weight(1f))
                StatusChip(a.marker.label, a.marker.tone)
            }
            Column(verticalArrangement = Arrangement.spacedBy(4.dp)) {
                summaryRows(a).forEach { (k, v) ->
                    Row(Modifier.fillMaxWidth()) {
                        Text(k, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant, modifier = Modifier.weight(0.38f))
                        Text(v, style = MaterialTheme.typography.bodyMedium, modifier = Modifier.weight(0.62f))
                    }
                }
            }

            SheetSection("Routers serving it", empty = "No router serves this apartment yet.", isEmpty = a.routers.isEmpty()) {
                a.routers.forEach { r -> LinkRow("${r.name} (${r.state})") { links.onRouter(r.id) } }
            }
            SheetSection("OLT and PON ports", empty = "No OLT serves this apartment yet.", isEmpty = a.olts.isEmpty()) {
                a.olts.forEach { o -> LinkRow("${o.name}${ponText(o.pons)}, ONUs ${onusText(o.onusOnline, o.onusTotal)}") { links.onOlt(o.id) } }
            }
            SheetSection("ONUs (${onusText(a.onusOnline, a.onusTotal)})", empty = "No ONU is installed in a house here yet.", isEmpty = a.onus.isEmpty()) {
                a.onus.forEach { n ->
                    val where = listOf(
                        n.house.takeIf { it.isNotBlank() }?.let { "house $it" },
                        n.customer.takeIf { it.isNotBlank() },
                        n.olt.takeIf { it.isNotBlank() }?.let { it + if (n.pon > 0) " PON ${n.pon}" else "" },
                    ).filterNotNull().joinToString(", ")
                    LinkRow("ONU ${n.serial.ifBlank { n.id }}${if (where.isNotEmpty()) ", $where" else ""}, ${n.status.ifBlank { if (n.online) "online" else "offline" }}") {
                        links.onOnu(n.accountId)
                    }
                }
            }
            if (a.other.isNotEmpty()) {
                SheetSection("Other equipment", empty = "", isEmpty = false) {
                    a.other.forEach { o ->
                        LinkRow("${o.label}, ${o.kind}${if (o.house.isNotBlank()) ", house ${o.house}" else ""}, ${o.status}") { links.onOther() }
                    }
                }
            }

            FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                FilledTonalButton(onClick = { links.onApartment(a.id) }) { Text("Open apartment", maxLines = 1) }
                val point = a.point
                if (point != null) {
                    OutlinedButton(onClick = { openMap(context, a.name, point) }) {
                        Icon(Icons.Default.Place, contentDescription = null, modifier = Modifier.size(18.dp))
                        Text("Open in Google Maps", maxLines = 1, modifier = Modifier.padding(start = 6.dp))
                    }
                } else if (canWrite) {
                    OutlinedButton(onClick = { links.onAddGps(a.id) }) { Text("Add GPS", maxLines = 1) }
                }
            }
        }
    }
}

@Composable
private fun SheetSection(title: String, empty: String, isEmpty: Boolean, content: @Composable () -> Unit) {
    Column(verticalArrangement = Arrangement.spacedBy(2.dp)) {
        Text(title, style = MaterialTheme.typography.labelLarge, color = MaterialTheme.colorScheme.onSurfaceVariant)
        if (isEmpty) Text(empty, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant) else content()
    }
}

@Composable
private fun LinkRow(text: String, onClick: () -> Unit) {
    TextButton(onClick = onClick, modifier = Modifier.fillMaxWidth(), contentPadding = PaddingValues(horizontal = 4.dp, vertical = 6.dp)) {
        Text(text, modifier = Modifier.fillMaxWidth(), style = MaterialTheme.typography.bodyMedium)
    }
}
