package ke.co.ftw.ftnadmin.data.networkmap

import ke.co.ftw.ftnadmin.domain.GpsPoint
import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable

/** GET /api/v1/network/map: every apartment with its GPS point (when it has one), summary counts and equipment. */
@Serializable
data class NetworkMap(
    val apartments: List<NetApartment> = emptyList(),
    @SerialName("with_gps") val withGps: Int = 0,
    @SerialName("without_gps") val withoutGps: Int = 0,
    val down: Int = 0,
    val partial: Int = 0,
    val ok: Int = 0,
)

@Serializable
data class NetHouses(val active: Int = 0, val total: Int = 0, val online: Int = 0)

/** A router that serves the apartment. [state] is up, down, paused or waiting. */
@Serializable
data class NetRouter(val id: String = "", val name: String = "", val state: String = "")

/** An OLT with the PON ports that serve the apartment and how many of the apartment's ONUs are online on it. */
@Serializable
data class NetOlt(
    val id: String = "",
    val name: String = "",
    val router: String = "",
    val pons: List<Int> = emptyList(),
    @SerialName("onus_online") val onusOnline: Int = 0,
    @SerialName("onus_total") val onusTotal: Int = 0,
)

/** An ONU installed in a house of the apartment. [status] is online or offline. */
@Serializable
data class NetOnu(
    val id: String = "",
    val serial: String = "",
    val model: String = "",
    val house: String = "",
    val customer: String = "",
    val online: Boolean = false,
    val status: String = "",
    val olt: String = "",
    val pon: Int = 0,
    @SerialName("account_id") val accountId: String = "",
)

/** Another equipment item (switch, splitter...) installed at a house of the apartment. */
@Serializable
data class NetOther(
    val id: String = "",
    val kind: String = "",
    val label: String = "",
    val status: String = "",
    val house: String = "",
)

@Serializable
data class NetApartment(
    val id: String = "",
    val name: String = "",
    val address: String = "",
    @SerialName("has_gps") val hasGps: Boolean = false,
    val lat: Double? = null,
    val lng: Double? = null,
    /** The server's marker state: ok, partial, down or unknown (the app derives the same from the counts, see markerState). */
    val state: String = "",
    @SerialName("maps_url") val mapsUrl: String = "",
    val houses: NetHouses = NetHouses(),
    val routers: List<NetRouter> = emptyList(),
    val olts: List<NetOlt> = emptyList(),
    val onus: List<NetOnu> = emptyList(),
    val other: List<NetOther> = emptyList(),
    @SerialName("onus_online") val onusOnline: Int = 0,
    @SerialName("onus_total") val onusTotal: Int = 0,
    @SerialName("open_alerts") val openAlerts: Int = 0,
) {
    /** The GPS point, or null when the apartment has none. 0,0 is a real point; only a missing lat or lng means "none". */
    val point: GpsPoint? get() = if (hasGps && lat != null && lng != null) GpsPoint(lat, lng) else null
}
