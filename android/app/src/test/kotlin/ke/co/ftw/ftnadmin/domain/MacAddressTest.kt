package ke.co.ftw.ftnadmin.domain

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class MacAddressTest {
    @Test fun acceptsEveryCommonSpelling() {
        val want = "AA:BB:CC:DD:EE:FE"
        listOf(
            "AA:BB:CC:DD:EE:FE", "aa:bb:cc:dd:ee:fe", "aa-bb-cc-dd-ee-fe", "aabb.ccdd.eefe",
            "aabbccddeefe", " AA BB CC DD EE FE ", "AABB-CCDD-EEFE",
        ).forEach { assertEquals(it, want, MacAddress.normalize(it)) }
    }

    @Test fun rejectsWrongLength() {
        assertEquals(MacResult.Invalid(MacProblem.BadLength), MacAddress.check("AA:BB:CC:DD:EE"))
        assertEquals(MacResult.Invalid(MacProblem.BadLength), MacAddress.check("AA:BB:CC:DD:EE:FE:00"))
    }

    @Test fun rejectsNonHex() {
        assertEquals(MacResult.Invalid(MacProblem.BadCharacters), MacAddress.check("GG:BB:CC:DD:EE:FE"))
        assertEquals(MacResult.Invalid(MacProblem.BadCharacters), MacAddress.check("AA/BB/CC/DD/EE/FE"))
    }

    @Test fun rejectsEmpty() {
        assertEquals(MacResult.Invalid(MacProblem.Empty), MacAddress.check("   "))
        assertNull(MacAddress.errorFor(""))
    }

    @Test fun rejectsReservedAddresses() {
        assertEquals(MacResult.Invalid(MacProblem.AllZero), MacAddress.check("00:00:00:00:00:00"))
        assertEquals(MacResult.Invalid(MacProblem.Broadcast), MacAddress.check("ff:ff:ff:ff:ff:ff"))
        assertEquals(MacResult.Invalid(MacProblem.Multicast), MacAddress.check("01:00:5E:00:00:01"))
    }

    @Test fun isValidAndErrorFor() {
        assertTrue(MacAddress.isValid("00:1A:2B:3C:4D:5E"))
        assertFalse(MacAddress.isValid("00:1A:2B"))
        assertEquals(MacProblem.BadLength.message, MacAddress.errorFor("00:1A:2B"))
    }
}
