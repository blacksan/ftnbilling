package ke.co.ftw.ftnadmin.ui

import ke.co.ftw.ftnadmin.data.routers.PortRole
import ke.co.ftw.ftnadmin.data.routers.PortRow
import ke.co.ftw.ftnadmin.data.routers.PortRules
import ke.co.ftw.ftnadmin.ui.routers.ports.PortsViewModel
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class PortRulesTest {
    private fun port(role: String, confirmDisable: Boolean = false, warn: String = "") =
        PortRow(name = "ether2", role = role, confirmDisable = confirmDisable, disableWarning = warn, roleWarning = "You may lose your WinBox session on this port.")

    @Test fun disablingOltNeedsTheTypedName() {
        val c = PortRules.disable(port(PortRole.OLT, confirmDisable = true, warn = "All customers on the OLT go offline."))
        assertTrue(c.needsName)
        assertEquals("All customers on the OLT go offline.", c.warning)
    }

    @Test fun disablingACustomerPortOnlyAsksForConfirmation() {
        assertFalse(PortRules.disable(port(PortRole.CUSTOMER, warn = "3 flats are on this port.")).needsName)
    }

    @Test fun movingAnAdminLanPortOrTakingTheOltNeedsTheName() {
        assertTrue(PortRules.role(port(PortRole.LAN), PortRole.UNUSED).needsName)
        assertTrue(PortRules.role(port(PortRole.UNUSED), PortRole.OLT).needsName)
        assertFalse(PortRules.role(port(PortRole.UNUSED), PortRole.CUSTOMER).needsName)
    }

    @Test fun roleChoicesNeverOfferWan() {
        val p = port(PortRole.UNUSED).copy(roleOptions = listOf("wan", "lan", "customer", "unused"))
        assertEquals(listOf("lan", "customer", "unused"), PortsViewModel.roleChoices(p))
    }
}
