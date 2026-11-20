package ke.co.ftw.ftnadmin.ui

import ke.co.ftw.ftnadmin.ui.routers.setup.pasteAdminHint
import ke.co.ftw.ftnadmin.ui.routers.setup.portsOffNotice
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

class PortsOffNoticeTest {
    @Test fun usesTheConfiguredPortNames() {
        assertEquals(
            "After setup, only ether1 (internet) and ether2 (admin) are on. Turn on the OLT and customer ports in the Ports tab, or customers stay offline.",
            portsOffNotice("ether1", "ether2"),
        )
        assertTrue(portsOffNotice("sfp-sfpplus2", "ether7, ether8").startsWith("After setup, only sfp-sfpplus2 (internet) and ether7 and ether8 (admin) are on."))
    }

    @Test fun blankNamesFallBackToTheDefaults() {
        assertTrue(portsOffNotice("", "").startsWith("After setup, only ether1 (internet) and ether2 (admin)"))
        assertTrue(pasteAdminHint("").contains("admin port (ether2)"))
    }
}
