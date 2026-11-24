package ke.co.ftw.ftnadmin.ui

import ke.co.ftw.ftnadmin.core.network.ApiJson
import ke.co.ftw.ftnadmin.data.equipment.OltStatus
import ke.co.ftw.ftnadmin.data.routers.PortRole
import ke.co.ftw.ftnadmin.ui.network.roleLabel
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class OltAdoptionModelTest {
    private val json = """{"configured":true,"connected":true,"online":3,"total":4,"adoption":{"stage":"adopted","next":"Secure this OLT on the web.","error":"","manual":false,"port":"ether5",
        "steps":[{"label":"Cable and port","state":"ok","detail":"Router port ether5"},{"label":"SSH reached","state":"ok","detail":""},{"label":"Serial check","state":"fail","detail":"Not this OLT"},{"label":"Secured","state":"todo","detail":""}],
        "secured":false,"snmp_off":false,"credentials":{"stored":true,"source":"stored","need_login":false}}}"""

    @Test fun adoptionStateReadsFromTheStatus() {
        val st = ApiJson.decodeFromString(OltStatus.serializer(), json)
        assertNotNull(st.adoption)
        val a = st.adoption!!
        assertEquals(2, a.doneCount)
        assertEquals(4, a.steps.size)
        assertTrue(a.failed)
        assertFalse(a.secured)
        assertEquals("ether5", a.port)
    }

    @Test fun anOlderServerHasNoAdoption() {
        val st = ApiJson.decodeFromString(OltStatus.serializer(), """{"configured":false}""")
        assertNull(st.adoption)
    }

    @Test fun theOltManagementRoleHasALabel() {
        assertEquals("OLT management", PortRole.label(PortRole.OLT_MGMT))
        assertEquals("OLT management", roleLabel("oltmgmt"))
    }
}
