package ke.co.ftw.ftnadmin.net

import org.junit.Test
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue

class JsonParsersTest {
    @Test
    fun parsesHealth() {
        val h = JsonParsers.parseHealth(
            """{"ok":true,"router_ok":true,"sites_total":3,"sites_online":2,"test_mode":false,"mpesa_enabled":true,"db":"postgres","db_ok":true}""",
        )!!
        assertTrue(h.ok)
        assertEquals(3, h.sitesTotal)
        assertEquals(2, h.sitesOnline)
        assertTrue(h.mpesaEnabled)
        assertFalse(h.testMode)
        assertEquals("postgres", h.db)
    }

    @Test
    fun healthWithMissingFieldsDefaultsToNotOk() {
        val h = JsonParsers.parseHealth("""{"ok":true}""")!!
        assertTrue(h.ok)
        assertFalse(h.routerOk)
        assertFalse(h.dbOk)
        assertEquals(0, h.sitesTotal)
    }

    @Test
    fun malformedBodiesAreNull() {
        assertNull(JsonParsers.parseHealth("<html>502</html>"))
        assertNull(JsonParsers.parseSummary(""))
    }

    @Test
    fun parsesSummary() {
        val s = JsonParsers.parseSummary(
            """
            {"v":1,"now":"2026-11-18T10:00:00Z","brand":"FTN","has_ops":true,
             "totals":{"sites":2,"up":1,"down":1,"other":0,"accounts_active":40,"accounts_total":55,
                       "revenue_today_kes":12500,"revenue_month_kes":300000,"open_alerts":2,"unmatched":1,
                       "unregistered":0,"failed_messages":3,"pending_orders":4},
             "sites":[{"id":"a","name":"Block A","state":"up","since":"","cause":"","handshake":"","wan":"",
                       "active":20,"total":27,"today_kes":5000,"month_kes":120000,"alerts":0,"muted":false},
                      {"id":"b","name":"Block B","state":"down","cause":"no ping","alerts":2,"muted":true}]}
            """.trimIndent(),
        )!!
        assertEquals(1, s.version)
        assertTrue(s.hasOps)
        assertEquals(300000L, s.totals.revenueMonthKes)
        assertEquals(2, s.totals.openAlerts)
        assertEquals(2, s.sites.size)
        assertEquals("Block A", s.sites[0].name)
        assertEquals("down", s.sites[1].state)
        assertTrue(s.sites[1].muted)
        assertEquals(0, s.sites[1].active)
    }

    @Test
    fun summaryWithoutSitesOrTotalsStillParses() {
        val s = JsonParsers.parseSummary("""{"v":1}""")
        assertNotNull(s)
        assertTrue(s!!.sites.isEmpty())
        assertEquals(0, s.totals.sites)
        assertEquals("unknown", JsonParsers.parseSummary("""{"sites":[{"id":"x"}]}""")!!.sites.single().state)
    }
}
