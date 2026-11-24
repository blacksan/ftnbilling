package ke.co.ftw.ftnadmin.ui.network

import ke.co.ftw.ftnadmin.data.networkmap.NetApartment
import ke.co.ftw.ftnadmin.data.networkmap.NetHouses
import ke.co.ftw.ftnadmin.data.networkmap.NetOlt
import ke.co.ftw.ftnadmin.data.networkmap.NetRouter
import ke.co.ftw.ftnadmin.ui.common.Tone

/**
 * How an apartment's marker looks. The same rules and words as the web Network page (netMarkerState in Go):
 * teal all good, neutral some offline, red router down.
 */
enum class MarkerState(val wire: String, val label: String) {
    Ok("ok", "All good"),
    Partial("partial", "Some offline"),
    Down("down", "Router down"),
    Unknown("unknown", "No router"),
    ;

    /** Worst first: down, some offline, all good, no router. */
    val rank: Int get() = when (this) {
        Down -> 3
        Partial -> 2
        Ok -> 1
        Unknown -> 0
    }

    val tone: Tone get() = when (this) {
        Ok -> Tone.Good
        Partial -> Tone.Neutral
        Down -> Tone.Bad
        Unknown -> Tone.Neutral
    }

    companion object {
        fun fromWire(s: String): MarkerState = entries.firstOrNull { it.wire == s } ?: Unknown
    }
}

/**
 * The marker state from the routers serving the apartment (state up, down, paused or waiting) and its ONU counts.
 *
 * - no router: [MarkerState.Unknown]
 * - routers serve it and none is up while one is down: [MarkerState.Down]
 * - a router is paused, not joined, or down while another is up, or some ONU is offline: [MarkerState.Partial]
 * - otherwise [MarkerState.Ok]
 */
fun markerState(routerStates: List<String>, onusOnline: Int, onusTotal: Int): MarkerState {
    if (routerStates.isEmpty()) return MarkerState.Unknown
    val up = routerStates.count { it == "up" }
    val down = routerStates.count { it == "down" }
    return when {
        up == 0 && down > 0 -> MarkerState.Down
        up != routerStates.size || onusOnline < onusTotal -> MarkerState.Partial
        else -> MarkerState.Ok
    }
}

/** The marker state of [a], worked out from its own counts (equal to the server's `state`). */
val NetApartment.marker: MarkerState get() = markerState(routers.map { it.state }, onusOnline, onusTotal)

/** Marker colours as ARGB longs: Lattice Teal 600, slate, and the error red. Independent of the app theme so a pin is always the same. */
fun MarkerState.argb(): Long = when (this) {
    MarkerState.Ok -> 0xFF1A6D6BL
    MarkerState.Partial -> 0xFF64748BL
    MarkerState.Down -> 0xFFC0283DL
    MarkerState.Unknown -> 0xFF8391A7L
}

fun plural(n: Int, one: String, many: String = one + "s"): String = "$n ${if (n == 1) one else many}"

fun housesText(h: NetHouses): String = "${h.active} active of ${h.total}"

fun onusText(online: Int, total: Int): String = "$online of $total online"

/** "Block A (up), Block B (down)", or "none". */
fun routersText(routers: List<NetRouter>): String =
    if (routers.isEmpty()) "none" else routers.joinToString(", ") { "${it.name} (${it.state})" }

/** "Hilltop OLT PON 1, 2 (3 of 4 online)"; "none" when no OLT serves the apartment. */
fun oltsText(olts: List<NetOlt>): String =
    if (olts.isEmpty()) "none" else olts.joinToString(", ") { o -> "${o.name}${ponText(o.pons)} (${onusText(o.onusOnline, o.onusTotal)})" }

/** " PON 1, 2" or "" when no port is known. */
fun ponText(pons: List<Int>): String = if (pons.isEmpty()) "" else " PON " + pons.joinToString(", ")

fun alertsText(n: Int): String = if (n == 0) "No open alerts" else plural(n, "open alert")

/** The label/value rows of the summary, in the order of the web hover card. */
fun summaryRows(a: NetApartment): List<Pair<String, String>> = listOf(
    "Address" to a.address.ifBlank { "No address" },
    "Houses" to housesText(a.houses),
    "Online now" to a.houses.online.toString(),
    "Routers" to routersText(a.routers),
    "OLTs" to oltsText(a.olts),
    "ONUs" to onusText(a.onusOnline, a.onusTotal),
    "Open alerts" to a.openAlerts.toString(),
)

/** One-line subtitle of a list row: "3 of 12 houses online, 2 ONUs offline". */
fun listSubtitle(a: NetApartment): String {
    val offline = a.onusTotal - a.onusOnline
    val houses = "${a.houses.online} of ${plural(a.houses.total, "house")} online"
    return if (offline > 0) "$houses, ${plural(offline, "ONU")} offline" else houses
}

/** Apartments with a GPS point (the map) and without (the "No location yet" list). */
fun splitByLocation(all: List<NetApartment>): Pair<List<NetApartment>, List<NetApartment>> =
    all.partition { it.point != null }

/** Worst state first, then by name: how the list view orders the apartments. */
fun sortedForList(all: List<NetApartment>): List<NetApartment> =
    all.sortedWith(compareByDescending<NetApartment> { it.marker.rank }.thenBy { it.name.lowercase() })

/** How many apartments are in each state, for the legend: down, partial, ok. */
fun stateCounts(all: List<NetApartment>): Triple<Int, Int, Int> =
    Triple(all.count { it.marker == MarkerState.Down }, all.count { it.marker == MarkerState.Partial }, all.count { it.marker == MarkerState.Ok })
