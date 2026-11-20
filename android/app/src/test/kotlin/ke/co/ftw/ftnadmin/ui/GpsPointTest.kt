package ke.co.ftw.ftnadmin.ui

import ke.co.ftw.ftnadmin.domain.Gps
import ke.co.ftw.ftnadmin.domain.GpsPoint
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

/** The GPS box and the map links of an apartment. Pure JVM: no Android classes. */
class GpsPointTest {
    private fun ok(s: String): GpsPoint = (GpsPoint.parse(s) as? Gps.Ok)?.point ?: error("not accepted: $s")

    private fun bad(s: String) = assertTrue("accepted: $s", GpsPoint.parse(s) is Gps.Bad)

    @Test fun plainPairsAsPastedFromGoogleMaps() {
        val want = GpsPoint(-1.2921, 36.8219)
        for (s in listOf("-1.2921, 36.8219", "-1.2921,36.8219", "  -1.2921 36.8219 ", "-1.2921; 36.8219")) assertEquals(s, want, ok(s))
        assertEquals(GpsPoint(0.0, 0.0), ok("0,0"))
        assertEquals(GpsPoint(90.0, 180.0), ok("90, 180"))
        assertEquals(GpsPoint(1.5, -179.9), ok("+1.5, -179.9"))
    }

    @Test fun googleMapsLinks() {
        val want = GpsPoint(-1.2921, 36.8219)
        assertEquals(want, ok("https://www.google.com/maps/@-1.2921,36.8219,15z"))
        assertEquals(want, ok("https://www.google.com/maps?q=-1.2921,36.8219"))
        assertEquals(want, ok("https://www.google.com/maps/search/?api=1&query=-1.2921%2C36.8219"))
        assertEquals(want, ok("https://maps.google.com/?q=-1.2921+36.8219"))
        assertEquals("the place pin beats the map centre", want, ok("https://www.google.com/maps/place/MG+One/@-1.30,36.80,17z/data=!3m1!4b1!8m2!3d-1.2921!4d36.8219"))
    }

    @Test fun emptyIsNoPointAndGarbageIsRefusedInWords() {
        assertEquals(Gps.Empty, GpsPoint.parse("   "))
        for (s in listOf("garbage", "-1.29", "1,2,3", "36.8219, -1.2921x", "https://maps.app.goo.gl/abc123", "https://example.com/?q=hello", "1e2, 3")) bad(s)
        assertEquals("Latitude must be between -90 and 90.", (GpsPoint.parse("91, 10") as Gps.Bad).message)
        assertEquals("Longitude must be between -180 and 180.", (GpsPoint.parse("10, -181") as Gps.Bad).message)
        assertEquals(GpsPoint.BAD, (GpsPoint.parse("somewhere") as Gps.Bad).message)
    }

    @Test fun textAndLinks() {
        val p = GpsPoint(-1.29210004, 36.8219)
        assertEquals("-1.2921,36.8219", p.text)
        assertEquals("0,0", GpsPoint(-0.0, 0.0).text)
        assertEquals("same URL the server sends as map_url", "https://www.google.com/maps/search/?api=1&query=-1.2921%2C36.8219", p.webUrl)
        assertEquals("geo:-1.2921,36.8219?q=-1.2921,36.8219(MG%20One%20Central)", p.geoUri("MG One Central"))
        assertEquals("a name with symbols is encoded", "geo:-1.2921,36.8219?q=-1.2921,36.8219(Tom%20%26%20Co%20%28B%29)", p.geoUri("Tom & Co (B)"))
        assertEquals("geo:-1.2921,36.8219?q=-1.2921,36.8219", p.geoUri("  "))
    }
}
