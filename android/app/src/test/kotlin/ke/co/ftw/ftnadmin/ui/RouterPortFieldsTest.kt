package ke.co.ftw.ftnadmin.ui

import ke.co.ftw.ftnadmin.data.routers.NetInterface
import ke.co.ftw.ftnadmin.data.sites.AutoInfo
import ke.co.ftw.ftnadmin.data.sites.CheckFix
import ke.co.ftw.ftnadmin.data.sites.CheckItem
import ke.co.ftw.ftnadmin.data.sites.CheckStatus
import ke.co.ftw.ftnadmin.data.sites.FixKind
import ke.co.ftw.ftnadmin.domain.CustomerPortStatus
import ke.co.ftw.ftnadmin.domain.customerPortLine
import ke.co.ftw.ftnadmin.ui.more.RouterFormCopy
import ke.co.ftw.ftnadmin.ui.more.portChoices
import kotlinx.serialization.json.Json
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class RouterPortFieldsTest {
    @Test fun formCopyIsTheAgreedWording() {
        assertEquals("Customer ports (extra)", RouterFormCopy.CUSTOMER_PORT_LABEL)
        assertEquals(
            "Ports where you can plug a customer device directly (for example ether4,ether5). The server sets them up.",
            RouterFormCopy.CUSTOMER_PORT_HINT,
        )
        assertEquals("OLT uplink port", RouterFormCopy.OLT_LABEL)
        assertEquals(
            "The port the OLT is plugged into: an SFP port (sfp-sfpplus1) or an Ethernet port (for example ether3).",
            RouterFormCopy.OLT_HINT,
        )
    }

    @Test fun applyStatusIsShownAsTheServerWritesIt() {
        val json = Json { ignoreUnknownKeys = true }
        val ok = json.decodeFromString<CustomerPortStatus>("""{"port":"ether5","state":"ok","text":"ether5: customer port ✓"}""")
        assertEquals("ether5: customer port ✓", customerPortLine(ok))
        val busy = CustomerPortStatus(port = "ether5", state = "applying", text = "ether5: applying…")
        assertEquals("ether5: applying…", customerPortLine(busy))
        val bad = CustomerPortStatus(port = "ether5", state = "error", text = "ether5: ether5 is the WAN port", message = "ether5 is the WAN port")
        assertEquals("ether5: ether5 is the WAN port", customerPortLine(bad))
        assertNull(customerPortLine(null))
        assertNull(customerPortLine(CustomerPortStatus()))
    }

    @Test fun portsResponseReadsTheApplyStatusAndTheOltCannotBeSwitchedOff() {
        val json = Json { ignoreUnknownKeys = true }
        val r = json.decodeFromString<ke.co.ftw.ftnadmin.data.routers.PortsResponse>(
            """{"items":[{"name":"sfp-sfpplus1","role":"olt","can_change":true,"can_disable":false},{"name":"ether4","role":"unused","can_change":true,"can_disable":true}],""" +
                """"customer_port":{"port":"ether5","state":"ok","text":"ether5: customer port ✓"}}""",
        )
        assertEquals("ether5: customer port ✓", customerPortLine(r.customerPort))
        assertFalse(r.items[0].canDisable)
        assertTrue(r.items[1].canDisable)
    }

    @Test fun portPickerOffersOnlyRealPorts() {
        val list = listOf("ether1", "ether3", "sfp-sfpplus1", "combo1", "br-customers", "vlan100-customers", "wg-ftn", "lo")
            .map { n -> NetInterface(name = n, type = if (n.startsWith("br-")) "bridge" else if (n.startsWith("vlan")) "vlan" else if (n.startsWith("wg")) "wg" else if (n == "lo") "loopback" else "ether") }
        assertEquals(listOf("ether1", "ether3", "sfp-sfpplus1", "combo1"), portChoices(list))
    }

    @Test fun theChecklistReadsTheAutoFixPause() {
        val json = Json { ignoreUnknownKeys = true }
        val c = json.decodeFromString<ke.co.ftw.ftnadmin.data.sites.Checks>("""{"items":[],"auto_fixes":{"paused":true,"until":"2026-11-20T08:00:00Z"}}""")
        assertTrue(c.autoFixes!!.paused)
        assertNull(json.decodeFromString<ke.co.ftw.ftnadmin.data.sites.Checks>("""{"items":[]}""").autoFixes)
    }

    @Test fun checkCardHidesBlankEvidenceAndTheButtonOfAutoFixes() {
        val blank = CheckItem(id = "a", status = CheckStatus.WARN, evidence = listOf("", "  ", "\t"))
        assertTrue(blank.shownEvidence.isEmpty())
        assertEquals(listOf("real line"), CheckItem(id = "b", evidence = listOf("", "real line ")).shownEvidence)

        val auto = CheckItem(id = "dhcp", status = CheckStatus.FAIL, fix = CheckFix(kind = FixKind.AUTO, label = "Fix", automatic = true))
        assertFalse(auto.showsFixButton)
        val failed = auto.copy(fix = auto.fix!!.copy(tryNow = true), auto = AutoInfo(state = "failed", text = "The server tried at 14:05 and could not fix it."))
        assertTrue(failed.showsFixButton)
        val confirm = CheckItem(id = "olt", status = CheckStatus.FAIL, fix = CheckFix(kind = FixKind.CONFIRM))
        assertTrue(confirm.showsFixButton)
        val oldServer = CheckItem(id = "x", status = CheckStatus.WARN, fix = CheckFix(kind = FixKind.AUTO))
        assertTrue(oldServer.showsFixButton) // an older server still gets its button
        assertFalse(auto.copy(status = CheckStatus.OK).showsFixButton)
    }
}
