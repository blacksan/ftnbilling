package ke.co.ftw.ftnadmin.domain

import java.net.URLEncoder
import kotlin.math.abs
import kotlin.math.round

/**
 * A building's GPS point in decimal degrees (WGS84). Pure Kotlin so it is unit tested on the JVM; the screen turns the
 * strings below into Intents. 0,0 is a real point: "no point" is a null [GpsPoint], never zero.
 *
 * The parsing mirrors the server (store.ParseGPS in Go), which checks again on save.
 */
data class GpsPoint(val lat: Double, val lng: Double) {
    /** "-1.2921,36.8219": up to 7 decimals (about 1 cm), no trailing zeros. */
    val text: String get() = "${trim(lat)},${trim(lng)}"

    /** The one-click Google Maps link; the same URL the server sends as `map_url`. */
    val webUrl: String get() = "https://www.google.com/maps/search/?api=1&query=${URLEncoder.encode(text, "UTF-8")}"

    /** `geo:` URI that any maps app opens, with a labelled pin. Spaces are %20 (not +) so the label reads right. */
    fun geoUri(label: String): String {
        val name = URLEncoder.encode(label.trim(), "UTF-8").replace("+", "%20")
        return if (name.isBlank()) "geo:$text?q=$text" else "geo:$text?q=$text($name)"
    }

    private fun trim(v: Double): String {
        val r = round(v * 1e7) / 1e7
        val s = r.toBigDecimal().stripTrailingZeros().toPlainString()
        return if (s == "-0") "0" else s
    }

    companion object {
        const val BAD = "Not understood. Paste the coordinates like -1.2921, 36.8219 or a Google Maps link that contains them."

        private const val NUM = "[-+]?\\d{1,3}(?:\\.\\d+)?"
        private val pair = Regex("^\\s*($NUM)\\s*[,;\\s]\\s*($NUM)\\s*$")
        private val at = Regex("@($NUM),($NUM)")
        private val data = Regex("!3d($NUM)!4d($NUM)")
        private val queryKeys = listOf("q", "query", "ll", "center", "destination", "daddr")

        /**
         * What people paste from Google Maps: "-1.2921, 36.8219" (comma, semicolon or space between the numbers), or a
         * Google Maps link with !3d..!4d.., @lat,lng or q/query/ll/center/destination=lat,lng. Blank is [Gps.Empty].
         */
        fun parse(input: String): Gps {
            val s = input.trim()
            if (s.isEmpty()) return Gps.Empty
            val nums: Pair<String, String>? = pair.matchEntire(s)?.let { it.groupValues[1] to it.groupValues[2] }
                ?: if (s.startsWith("http://", true) || s.startsWith("https://", true)) fromUrl(s) else null
            nums ?: return Gps.Bad(BAD)
            val lat = nums.first.toDoubleOrNull() ?: return Gps.Bad(BAD)
            val lng = nums.second.toDoubleOrNull() ?: return Gps.Bad(BAD)
            if (abs(lat) > 90) return Gps.Bad("Latitude must be between -90 and 90.")
            if (abs(lng) > 180) return Gps.Bad("Longitude must be between -180 and 180.")
            return Gps.Ok(GpsPoint(lat, lng))
        }

        private fun fromUrl(url: String): Pair<String, String>? {
            // The exact pin (!3d..!4d..) beats the map centre (@..), which beats search parameters.
            for (re in listOf(data, at)) re.find(url)?.let { return it.groupValues[1] to it.groupValues[2] }
            val query = url.substringAfter('?', "").substringBefore('#')
            for (kv in query.split('&')) {
                val key = kv.substringBefore('=')
                if (key !in queryKeys) continue
                val v = java.net.URLDecoder.decode(kv.substringAfter('=', ""), "UTF-8")
                pair.matchEntire(v)?.let { return it.groupValues[1] to it.groupValues[2] }
            }
            return null
        }
    }
}

/** Result of reading the GPS field. */
sealed interface Gps {
    /** Nothing typed: no point. */
    data object Empty : Gps

    data class Ok(val point: GpsPoint) : Gps

    data class Bad(val message: String) : Gps
}
