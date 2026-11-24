package ke.co.ftw.ftnadmin.ui

import ke.co.ftw.ftnadmin.ui.common.alertStateLabel
import ke.co.ftw.ftnadmin.ui.common.countLabel
import ke.co.ftw.ftnadmin.ui.common.formatPhone
import ke.co.ftw.ftnadmin.ui.common.houseText
import ke.co.ftw.ftnadmin.ui.common.leaseLabel
import ke.co.ftw.ftnadmin.ui.common.paymentMethodLabel
import ke.co.ftw.ftnadmin.ui.common.userRoleLabel
import org.junit.Assert.assertEquals
import org.junit.Test

class UiLabelsTest {
    @Test fun paymentMethodsUseFriendlyNames() {
        assertEquals("M-Pesa", paymentMethodLabel("mpesa"))
        assertEquals("M-Pesa", paymentMethodLabel(" MPESA "))
        assertEquals("Cash", paymentMethodLabel("cash"))
        assertEquals("Bank transfer", paymentMethodLabel("bank_transfer"))
        assertEquals("Card", paymentMethodLabel("card"))
        assertEquals("", paymentMethodLabel(""))
    }

    @Test fun rolesAndStatesAreCapitalisedWords() {
        assertEquals("Owner", userRoleLabel("owner"))
        assertEquals("Read only", userRoleLabel("read_only"))
        assertEquals("Firing", alertStateLabel("firing"))
    }

    @Test fun houseTextSaysHouseOnceAndNothingWhenBlank() {
        assertEquals("House B4", houseText(" B4 "))
        assertEquals("", houseText("  "))
    }

    @Test fun phonesAreGroupedForReading() {
        assertEquals("0712 345 678", formatPhone("254712345678"))
        assertEquals("0712 345 678", formatPhone("0712345678"))
        assertEquals("0712 345 678", formatPhone("712345678"))
        assertEquals("+44 20 7946 0000", formatPhone(" +44 20 7946 0000 "))
    }

    @Test fun countsPluralise() {
        assertEquals("1 house", countLabel(1, "house"))
        assertEquals("3 houses", countLabel(3, "house"))
        assertEquals("0 ports", countLabel(0, "port"))
    }

    @Test fun leaseWords() {
        assertEquals("Bound", leaseLabel("bound"))
        assertEquals("None", leaseLabel(""))
        assertEquals("Waiting for an address", leaseLabel("waiting"))
    }
}
