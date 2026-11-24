package ke.co.ftw.ftnadmin.ui

import ke.co.ftw.ftnadmin.domain.Account
import ke.co.ftw.ftnadmin.domain.AptRef
import org.junit.Assert.assertEquals
import org.junit.Test

class AccountRowTextTest {
    @Test fun nameIsTheHeadlineAndPlaceIsTheSupportingLine() {
        val a = Account(tenant = "Jane Wanjiku", unit = "A4", code = "ABC123", apartment = AptRef("1", "MG One Central"))
        assertEquals("Jane Wanjiku", a.headline)
        assertEquals("MG One Central  ·  House A4  ·  ABC123", a.placeLine)
    }

    @Test fun withoutNameTheHouseNumberIsTheHeadlineAndEmptyPartsAreLeftOut() {
        val a = Account(unit = "B2", code = "XYZ789")
        assertEquals("B2", a.headline)
        assertEquals("House B2  ·  XYZ789", a.placeLine)
    }
}
