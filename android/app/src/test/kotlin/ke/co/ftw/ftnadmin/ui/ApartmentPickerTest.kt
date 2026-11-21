package ke.co.ftw.ftnadmin.ui

import ke.co.ftw.ftnadmin.domain.APARTMENT_SEARCH_FROM
import ke.co.ftw.ftnadmin.domain.Apartment
import ke.co.ftw.ftnadmin.domain.ApartmentRouter
import ke.co.ftw.ftnadmin.domain.matching
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

class ApartmentPickerTest {
    private val rift = Apartment(
        id = "a1", name = "Rift View", address = "Ngong Road", accountsTotal = 12,
        routers = listOf(ApartmentRouter(routerId = "r1", routerName = "Alpha"), ApartmentRouter(routerId = "r2", routerName = "Beta")),
    )
    private val mg = Apartment(id = "a2", name = "MG One Central", address = "Thika Road", accountsTotal = 1, routers = listOf(ApartmentRouter(routerId = "r2", routerName = "Beta")))
    private val empty = Apartment(id = "a3", name = "Green Court", accountsTotal = 0)
    private val all = listOf(rift, mg, empty)

    @Test
    fun blankQueryKeepsEverything() {
        assertEquals(all, all.matching(""))
        assertEquals(all, all.matching("   "))
    }

    @Test
    fun searchesNameAddressAndRouter() {
        assertEquals(listOf(mg), all.matching("mg one"))
        assertEquals(listOf(rift), all.matching("ngong"))
        assertEquals(listOf(rift, mg), all.matching("beta"))
        assertEquals(listOf(rift), all.matching("rift alpha"))
        assertTrue(all.matching("nothing here").isEmpty())
    }

    @Test
    fun summaryNamesSubscribersAndRouters() {
        assertEquals("12 subscribers  |  Router: Alpha, Beta", rift.pickerSummary)
        assertEquals("1 subscriber  |  Router: Beta", mg.pickerSummary)
        assertEquals("0 subscribers  |  No router yet", empty.pickerSummary)
    }

    @Test
    fun searchBoxThreshold() {
        assertEquals(8, APARTMENT_SEARCH_FROM)
    }
}
