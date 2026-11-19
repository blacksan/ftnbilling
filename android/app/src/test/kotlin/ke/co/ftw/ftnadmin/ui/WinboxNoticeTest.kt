package ke.co.ftw.ftnadmin.ui

import ke.co.ftw.ftnadmin.core.network.ApiJson
import ke.co.ftw.ftnadmin.domain.Site
import ke.co.ftw.ftnadmin.ui.routers.winboxNotice
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Test

class WinboxNoticeTest {
    @Test fun noTunnelAddressMeansNoNotice() {
        assertNull(winboxNotice("", true))
        assertNull(winboxNotice("", false))
    }

    @Test fun builtWithWinboxShowsTheConnectionLine() {
        assertEquals(
            "Remote WinBox: connect your management WireGuard profile, then open WinBox to 10.99.0.2",
            winboxNotice("10.99.0.2", true),
        )
    }

    @Test fun noReportOfWinboxSuggestsResetAndANewLine() {
        assertEquals("No report that this router has remote WinBox. If it was set up before remote WinBox existed, reset it and paste a new line.", winboxNotice("10.99.0.2", false))
    }

    @Test fun siteJsonCarriesTheTwoFieldsAndDefaultsWhenAbsent() {
        val with = ApiJson.decodeFromString(Site.serializer(), """{"router_id":"r1","tunnel_address":"10.99.0.2","mgmt_winbox":true}""")
        assertEquals("10.99.0.2", with.tunnelAddress)
        assertEquals(true, with.mgmtWinbox)
        val without = ApiJson.decodeFromString(Site.serializer(), """{"router_id":"r1"}""")
        assertEquals("", without.tunnelAddress)
        assertFalse(without.mgmtWinbox)
    }
}
