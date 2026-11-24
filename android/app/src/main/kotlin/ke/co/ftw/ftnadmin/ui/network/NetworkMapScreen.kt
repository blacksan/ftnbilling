package ke.co.ftw.ftnadmin.ui.network

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
import androidx.compose.runtime.rememberUpdatedState
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.compose.runtime.DisposableEffect
import androidx.compose.ui.viewinterop.AndroidView
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.LifecycleEventObserver
import androidx.lifecycle.compose.LocalLifecycleOwner
import org.maplibre.android.MapLibre
import org.maplibre.android.camera.CameraUpdateFactory
import org.maplibre.android.geometry.LatLng
import org.maplibre.android.geometry.LatLngBounds
import org.maplibre.android.maps.MapLibreMap
import org.maplibre.android.maps.MapView
import org.maplibre.android.maps.Style
import org.maplibre.android.style.expressions.Expression
import org.maplibre.android.style.layers.CircleLayer
import org.maplibre.android.style.layers.PropertyFactory
import org.maplibre.android.style.layers.SymbolLayer
import org.maplibre.android.style.sources.GeoJsonOptions
import org.maplibre.android.style.sources.GeoJsonSource
import org.maplibre.geojson.Feature
import org.maplibre.geojson.FeatureCollection
import org.maplibre.geojson.Point
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
            ui.mode == NetworkMode.MapView && ui.located.isNotEmpty() ->
                Column(Modifier.fillMaxSize()) {
                    Box(Modifier.weight(1f).fillMaxWidth()) { MapView(ui.located, onSelect = vm::select, onBackground = vm::dismiss) }
                    if (ui.unlocated.isNotEmpty()) NoLocationPanel(ui.unlocated, canWrite, links, Modifier.fillMaxWidth())
                }
            else -> PullToRefreshBox(ui.refreshing, vm::refresh, Modifier.fillMaxSize()) {
                ApartmentList(ui, vm, canWrite, links, showNoGpsNote = ui.mode == NetworkMode.MapView)
            }
        }
    }
    ui.selected?.let { a -> ApartmentSheet(a, canWrite, links, onDismiss = vm::dismiss) }
}

/** OpenFreeMap's calm light style: no key, no account. Its sprite, glyphs and tiles are all on tiles.openfreemap.org. */
private const val STYLE_URL = "https://tiles.openfreemap.org/styles/positron"
private const val SOURCE = "apartments"
private const val L_CLUSTER = "apartments-cluster"
private const val L_COUNT = "apartments-count"
private const val L_PIN = "apartments-pin"
private const val L_DOT = "apartments-dot"
private const val CLUSTER_MAX_ZOOM = 17
private const val MAX_ZOOM = 19.0
private const val TAP_SLOP_DP = 24f

/** One feature per apartment with a point; `rank` lets a cluster take the worst state of its members. */
private fun featuresOf(located: List<NetApartment>): FeatureCollection =
    FeatureCollection.fromFeatures(
        located.mapNotNull { a ->
            val p = a.point ?: return@mapNotNull null
            Feature.fromGeometry(Point.fromLngLat(p.lng, p.lat)).also {
                it.addStringProperty("id", a.id)
                it.addNumberProperty("rank", a.marker.rank)
            }
        },
    )

/** `match(rank)` over the Lattice Teal / slate / red set of [argb]: the same colours as the web map. */
private fun colourByRank(): Expression {
    val stops = MarkerState.entries.flatMap { listOf(Expression.literal(it.rank), Expression.color(it.argb().toInt())) }
    return Expression.match(Expression.get("rank"), Expression.color(MarkerState.Unknown.argb().toInt()), *stops.toTypedArray())
}

/**
 * The map: MapLibre Native in an AndroidView on the OpenFreeMap style. Pins are circle layers on one clustered GeoJSON source
 * (teal / slate / red by [MarkerState], a white ring and centre like before); close pins merge into a count bubble in the
 * worst state of its members. A tap on a pin selects the apartment, a tap on a bubble zooms in, a tap elsewhere dismisses.
 *
 * AndroidView over the stable SDK is used instead of the community maplibre-compose wrapper: the wrapper is a young,
 * fast-moving multiplatform library, and everything needed here (one source, four layers, three taps) is a screenful of calls.
 */
@Composable
private fun MapView(located: List<NetApartment>, onSelect: (String) -> Unit, onBackground: () -> Unit) {
    val context = LocalContext.current
    val lifecycle = LocalLifecycleOwner.current.lifecycle
    val mapView = remember {
        MapLibre.getInstance(context)
        MapView(context)
    }
    var map by remember { mutableStateOf<MapLibreMap?>(null) }
    var styled by remember { mutableStateOf(false) }
    val select by rememberUpdatedState(onSelect)
    val dismiss by rememberUpdatedState(onBackground)
    val density = context.resources.displayMetrics.density

    DisposableEffect(lifecycle, mapView) {
        mapView.onCreate(null)
        val observer = LifecycleEventObserver { _, e ->
            when (e) {
                Lifecycle.Event.ON_START -> mapView.onStart()
                Lifecycle.Event.ON_RESUME -> mapView.onResume()
                Lifecycle.Event.ON_PAUSE -> mapView.onPause()
                Lifecycle.Event.ON_STOP -> mapView.onStop()
                else -> Unit
            }
        }
        lifecycle.addObserver(observer)
        // The composable can enter after the activity is already started or resumed.
        if (lifecycle.currentState.isAtLeast(Lifecycle.State.STARTED)) mapView.onStart()
        if (lifecycle.currentState.isAtLeast(Lifecycle.State.RESUMED)) mapView.onResume()
        onDispose {
            lifecycle.removeObserver(observer)
            if (lifecycle.currentState.isAtLeast(Lifecycle.State.RESUMED)) mapView.onPause()
            if (lifecycle.currentState.isAtLeast(Lifecycle.State.STARTED)) mapView.onStop()
            mapView.onDestroy()
        }
    }

    LaunchedEffect(mapView) {
        mapView.getMapAsync { m ->
            m.setMaxZoomPreference(MAX_ZOOM)
            m.uiSettings.isRotateGesturesEnabled = false
            m.uiSettings.isTiltGesturesEnabled = false
            m.setStyle(Style.Builder().fromUri(STYLE_URL)) { style ->
                style.addSource(
                    GeoJsonSource(
                        SOURCE, FeatureCollection.fromFeatures(emptyList<Feature>()),
                        GeoJsonOptions()
                            .withCluster(true).withClusterRadius(44).withClusterMaxZoom(CLUSTER_MAX_ZOOM)
                            .withClusterProperty("rank", Expression.max(Expression.accumulated(), Expression.get("rank")), Expression.get("rank")),
                    ),
                )
                style.addLayer(
                    CircleLayer(L_CLUSTER, SOURCE).withFilter(Expression.has("point_count")).withProperties(
                        PropertyFactory.circleColor(colourByRank()),
                        PropertyFactory.circleRadius(18f),
                        PropertyFactory.circleStrokeColor(android.graphics.Color.WHITE),
                        PropertyFactory.circleStrokeWidth(3f),
                    ),
                )
                style.addLayer(
                    SymbolLayer(L_COUNT, SOURCE).withFilter(Expression.has("point_count")).withProperties(
                        PropertyFactory.textField(Expression.toString(Expression.get("point_count"))),
                        PropertyFactory.textFont(arrayOf("Noto Sans Bold")),
                        PropertyFactory.textSize(14f),
                        PropertyFactory.textColor(android.graphics.Color.WHITE),
                        PropertyFactory.textAllowOverlap(true),
                        PropertyFactory.textIgnorePlacement(true),
                    ),
                )
                style.addLayer(
                    CircleLayer(L_PIN, SOURCE).withFilter(Expression.not(Expression.has("point_count"))).withProperties(
                        PropertyFactory.circleColor(colourByRank()),
                        PropertyFactory.circleRadius(13f),
                        PropertyFactory.circleStrokeColor(android.graphics.Color.WHITE),
                        PropertyFactory.circleStrokeWidth(3f),
                    ),
                )
                style.addLayer(
                    CircleLayer(L_DOT, SOURCE).withFilter(Expression.not(Expression.has("point_count"))).withProperties(
                        PropertyFactory.circleColor(android.graphics.Color.WHITE),
                        PropertyFactory.circleRadius(4f),
                    ),
                )
                m.addOnMapClickListener { at ->
                    val pt = m.projection.toScreenLocation(at)
                    val slop = TAP_SLOP_DP * density / 2
                    val box = android.graphics.RectF(pt.x - slop, pt.y - slop, pt.x + slop, pt.y + slop)
                    val pin = m.queryRenderedFeatures(box, L_PIN).firstOrNull()
                    val bubble = m.queryRenderedFeatures(box, L_CLUSTER).firstOrNull()
                    when {
                        pin != null -> pin.getStringProperty("id")?.let { select(it) } ?: dismiss()
                        bubble != null -> {
                            val src = style.getSourceAs<GeoJsonSource>(SOURCE)
                            val zoom = src?.getClusterExpansionZoom(bubble)?.toDouble() ?: (m.cameraPosition.zoom + 2)
                            val center = (bubble.geometry() as? Point)?.let { LatLng(it.latitude(), it.longitude()) } ?: at
                            m.animateCamera(CameraUpdateFactory.newLatLngZoom(center, minOf(zoom, MAX_ZOOM)))
                        }
                        else -> dismiss()
                    }
                    true
                }
                map = m
                styled = true
            }
        }
    }

    val ids = located.map { it.id }
    LaunchedEffect(styled, located) {
        val m = map ?: return@LaunchedEffect
        if (!styled) return@LaunchedEffect
        m.getStyle { it.getSourceAs<GeoJsonSource>(SOURCE)?.setGeoJson(featuresOf(located)) }
    }
    LaunchedEffect(styled, ids) {
        val m = map ?: return@LaunchedEffect
        if (!styled) return@LaunchedEffect
        val points = located.mapNotNull { a -> a.point?.let { LatLng(it.lat, it.lng) } }
        if (points.isEmpty()) return@LaunchedEffect
        m.moveCamera(
            if (points.size == 1) {
                CameraUpdateFactory.newLatLngZoom(points[0], 16.0)
            } else {
                CameraUpdateFactory.newLatLngBounds(LatLngBounds.Builder().includes(points).build(), (60 * density).toInt())
            },
        )
    }

    AndroidView(factory = { mapView }, modifier = Modifier.fillMaxSize())
}

/** The List view, and what the Map view shows when no apartment has a point yet. */
@Composable
private fun ApartmentList(ui: NetworkMapUiState, vm: NetworkMapViewModel, canWrite: Boolean, links: NetworkLinks, showNoGpsNote: Boolean) {
    val context = LocalContext.current
    val (down, partial, ok) = stateCounts(ui.apartments)
    LazyColumn(
        Modifier.fillMaxSize(),
        contentPadding = PaddingValues(start = 16.dp, end = 16.dp, top = 4.dp, bottom = 24.dp),
        verticalArrangement = Arrangement.spacedBy(8.dp),
    ) {
        if (showNoGpsNote) {
            item(key = "note") {
                Text(
                    "No apartment has a GPS point yet, so there is nothing to put on the map.",
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
