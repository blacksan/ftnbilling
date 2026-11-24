package ke.co.ftw.ftnadmin.ui

import ke.co.ftw.ftnadmin.core.network.ApiClient
import ke.co.ftw.ftnadmin.core.network.ApiJson
import ke.co.ftw.ftnadmin.core.network.MemoryTokenStore
import ke.co.ftw.ftnadmin.core.network.Session
import ke.co.ftw.ftnadmin.core.network.SessionManager
import ke.co.ftw.ftnadmin.core.network.UserInfo
import ke.co.ftw.ftnadmin.data.equipment.EquipmentRepository
import ke.co.ftw.ftnadmin.data.equipment.EquipmentTree
import ke.co.ftw.ftnadmin.data.onu.RegisterItem
import ke.co.ftw.ftnadmin.domain.ItemList
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.equipment.EquipmentTab
import ke.co.ftw.ftnadmin.ui.equipment.EquipmentTabCounts
import ke.co.ftw.ftnadmin.ui.equipment.EquipmentTabsViewModel
import ke.co.ftw.ftnadmin.ui.equipment.equipmentTabCounts
import ke.co.ftw.ftnadmin.ui.equipment.filterOnuRows
import ke.co.ftw.ftnadmin.ui.equipment.filterOther
import ke.co.ftw.ftnadmin.ui.equipment.isOtherKind
import ke.co.ftw.ftnadmin.ui.equipment.oltRows
import ke.co.ftw.ftnadmin.ui.equipment.onuPlacement
import ke.co.ftw.ftnadmin.ui.equipment.onuRows
import ke.co.ftw.ftnadmin.ui.equipment.onuStatusLabel
import ke.co.ftw.ftnadmin.ui.equipment.otherItems
import ke.co.ftw.ftnadmin.ui.equipment.tabTitle
import ke.co.ftw.ftnadmin.ui.nav.EquipmentRoute
import ke.co.ftw.ftnadmin.ui.nav.EquipmentTreeRoute
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.test.TestScope
import kotlinx.coroutines.test.advanceUntilIdle
import kotlinx.coroutines.test.runTest
import kotlinx.serialization.json.Json
import okhttp3.mockwebserver.Dispatcher
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import okhttp3.mockwebserver.RecordedRequest
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Rule
import org.junit.Test

/** Equipment with four tabs: the tab list, the routes, the lists each tab shows and the view model. Every helper used here is defined here. */
@OptIn(ExperimentalCoroutinesApi::class)
class EquipmentTabsTest {
    @get:Rule val main = MainDispatcherRule()
    private lateinit var server: MockWebServer
    private lateinit var api: ApiClient
    private val routes = mutableMapOf<String, MockResponse>()

    @Before fun setUp() {
        server = MockWebServer()
        server.dispatcher = object : Dispatcher() {
            override fun dispatch(request: RecordedRequest): MockResponse {
                val key = "${request.method} ${request.requestUrl!!.encodedPath}"
                return routes[key] ?: MockResponse().setResponseCode(404).setBody("""{"error":{"code":"not_found","message":"nope"}}""")
            }
        }
        server.start()
        val sessions = SessionManager(MemoryTokenStore(Session("a", "", "r", "", "d", UserInfo("1", "b", "owner"))))
        sessions.restore()
        api = ApiClient.create(server.url("/").toString(), sessions, io = main.dispatcher)
    }

    @After fun tearDown() {
        main.cancelViewModels()
        server.shutdown()
    }

    private fun <T> TestScope.await(flow: StateFlow<T>, cond: (T) -> Boolean) {
        advanceUntilIdle()
        val end = System.currentTimeMillis() + 5_000
        while (!cond(flow.value) && System.currentTimeMillis() < end) {
            Thread.sleep(20)
            advanceUntilIdle()
        }
        assertTrue("state condition not reached: ${flow.value}", cond(flow.value))
    }

    private val onu1 = """{"id":"n1","serial":"ZTEG1","model":"F670L","mac":"AA:BB:CC:00:00:01","status":"installed","olt_id":"o1","pon_port":2,
        "house":{"account_id":"a1","code":"FTN-1","unit":"A1","apartment_id":"ap1","apartment":"Hilltop","customer":"Wanjiru"},
        "online":true,"tr069":{"linked":true,"last_inform":"2026-11-20T10:00:00Z","held_back":false},"warnings":[]}"""
    private val onu2 = """{"id":"n2","serial":"ZTEG2","mac":"AA:BB:CC:00:00:02","status":"in_stock","online":false,"warnings":[]}"""
    private val olt1 = """{"id":"o1","name":"Hilltop OLT","model":"V1600G","mac":"AA:BB:CC:00:0F:01","status":"installed","router_id":"r1","router_port":"ether5",
        "pon_ports":4,"onu_count":1,"online_count":1,"pons":[{"port":2,"apartments":[{"id":"ap1","name":"Hilltop"}],"onus":[$onu1]}]}"""
    private val olt2 = """{"id":"o2","name":"Spare OLT","mac":"AA:BB:CC:00:0F:02","status":"in_stock","pons":[]}"""
    private val treeJson = """{"routers":[{"id":"r1","name":"Kilimani","olts":[$olt1],"onu_count":1,"online_count":1}],"unplaced_olts":[$olt2],"unplaced_onus":[$onu2]}"""
    private val registerJson = """{"items":[
        {"id":"n1","kind":"ont","serial":"ZTEG1","mac":"AA:BB:CC:00:00:01","status":"installed"},
        {"id":"o1","kind":"olt","serial":"VS1","mac":"AA:BB:CC:00:0F:01","status":"installed"},
        {"id":"s1","kind":"switch","serial":"SW1","mac":"AA:BB:CC:00:5E:01","model":"CRS326","status":"in_stock"},
        {"id":"p1","kind":"splitter","serial":"SP1","model":"1x8","status":"installed"}]}"""

    private fun tree() = ApiJson.decodeFromString(EquipmentTree.serializer(), treeJson)
    private fun register() = ApiJson.decodeFromString(ItemList.serializer(RegisterItem.serializer()), registerJson).items

    // ---- tabs and routes ----

    @Test fun fourTabsInOrderWithRoutersAsTheDefault() {
        assertEquals(listOf("Routers", "OLT", "ONU", "Other"), EquipmentTab.entries.map { it.label })
        assertEquals(listOf("routers", "olt", "onu", "other"), EquipmentTab.entries.map { it.key })
        assertEquals(EquipmentTab.Routers, EquipmentTab.fromKey(null))
        assertEquals(EquipmentTab.Routers, EquipmentTab.fromKey("nonsense"))
        assertEquals(EquipmentTab.Onu, EquipmentTab.fromKey("onu"))
        assertEquals(EquipmentTab.Other, EquipmentTab.fromKey("other"))
    }

    @Test fun equipmentRouteCarriesTheTabAndSurvivesSerialisation() {
        assertEquals("routers", EquipmentRoute().tab)
        val json = Json.encodeToString(EquipmentRoute.serializer(), EquipmentRoute("olt"))
        assertEquals("olt", Json.decodeFromString(EquipmentRoute.serializer(), json).tab)
        assertEquals(EquipmentTab.Olt, EquipmentTab.fromKey(Json.decodeFromString(EquipmentRoute.serializer(), json).tab))
        // the tree is its own destination (a different route), so Back from it returns to the tabs
        assertTrue(EquipmentTreeRoute.toString() != EquipmentRoute().toString())
    }

    // ---- what each tab lists ----

    @Test fun otherExcludesOltAndOnu() {
        val all = register()
        assertFalse(isOtherKind("olt"))
        assertFalse(isOtherKind("ont"))
        assertTrue(isOtherKind("switch"))
        assertEquals(listOf("SW1", "SP1"), otherItems(all).map { it.serial })
    }

    @Test fun oltAndOnuRowsFlattenTheTree() {
        val t = tree()
        assertEquals(listOf("Hilltop OLT" to "Kilimani", "Spare OLT" to ""), t.oltRows().map { it.olt.name to it.routerName })
        val onus = t.onuRows()
        assertEquals(listOf("ZTEG1", "ZTEG2"), onus.map { it.onu.serial })
        assertEquals("Hilltop OLT", onus[0].oltName)
        assertEquals("Kilimani", onus[0].routerName)
        assertEquals("", onus[1].oltName)
        assertEquals("Kilimani  |  Hilltop OLT  |  PON 2", onuPlacement(onus[0]))
        assertEquals("Not placed on an OLT", onuPlacement(onus[1]))
    }

    @Test fun onuFiltersByStatusOltAndSearch() {
        val rows = tree().onuRows()
        assertEquals(2, filterOnuRows(rows, "", "", "").size)
        assertEquals(listOf("ZTEG1"), filterOnuRows(rows, "", "installed", "").map { it.onu.serial })
        assertEquals(listOf("ZTEG2"), filterOnuRows(rows, "", "in_stock", "").map { it.onu.serial })
        assertEquals(listOf("ZTEG1"), filterOnuRows(rows, "", "", "o1").map { it.onu.serial })
        assertEquals(listOf("ZTEG2"), filterOnuRows(rows, "zteg2", "", "").map { it.onu.serial })
        assertEquals(listOf("ZTEG1"), filterOnuRows(rows, "aa:bb:cc:00:00:01", "", "").map { it.onu.serial })
        assertEquals(listOf("ZTEG1"), filterOnuRows(rows, "A1", "", "").map { it.onu.serial })
        assertEquals(listOf("ZTEG1"), filterOnuRows(rows, "wanjiru", "", "").map { it.onu.serial })
        assertTrue(filterOnuRows(rows, "nomatch", "", "").isEmpty())
        assertTrue(filterOnuRows(rows, "", "faulty", "").isEmpty())
    }

    @Test fun otherSearchMatchesKindSerialModelAndMac() {
        val other = otherItems(register())
        assertEquals(2, filterOther(other, "").size)
        assertEquals(listOf("SW1"), filterOther(other, "crs").map { it.serial })
        assertEquals(listOf("SP1"), filterOther(other, "splitter").map { it.serial })
        assertEquals(listOf("SW1"), filterOther(other, "aa:bb:cc:00:5e:01").map { it.serial })
    }

    @Test fun statusWordsReadAsDeployedAndInStock() {
        assertEquals("Deployed", onuStatusLabel("installed"))
        assertEquals("In stock", onuStatusLabel("in_stock"))
        assertEquals("Faulty", onuStatusLabel("faulty"))
        assertEquals("Returned", onuStatusLabel("returned"))
    }

    // ---- counts ----

    @Test fun tabCountsComeFromWhatHasLoaded() {
        val c = equipmentTabCounts(routers = 3, tree = tree(), other = register())
        assertEquals(EquipmentTabCounts(routers = 3, olts = 2, onus = 2, other = 2), c)
        assertEquals("Routers (3)", tabTitle(EquipmentTab.Routers, c))
        assertEquals("OLT (2)", tabTitle(EquipmentTab.Olt, c))
        assertEquals("ONU (2)", tabTitle(EquipmentTab.Onu, c))
        assertEquals("Other (2)", tabTitle(EquipmentTab.Other, c))
        // before anything has loaded the tabs show their names only
        val none = equipmentTabCounts(null, null, null)
        assertNull(none.of(EquipmentTab.Olt))
        assertEquals("OLT", tabTitle(EquipmentTab.Olt, none))
    }

    // ---- the view model ----

    @Test fun viewModelStartsOnTheRouteTabAndLoadsOtherWithoutOltAndOnu() = runTest(main.dispatcher) {
        routes["GET /api/v1/equipment"] = MockResponse().setBody(registerJson)
        val vm = main.track(EquipmentTabsViewModel(EquipmentRepository(api), EquipmentTab.Onu))
        assertEquals(EquipmentTab.Onu, vm.state.value.tab)
        await(vm.state) { it.register is Loadable.Ready }
        val other = vm.state.value.other as Loadable.Ready
        assertEquals(listOf("SW1", "SP1"), other.value.map { it.serial })
        vm.selectTab(EquipmentTab.Other)
        assertEquals(EquipmentTab.Other, vm.state.value.tab)
    }

    @Test fun viewModelFilterTogglesClearOnSecondTap() = runTest(main.dispatcher) {
        routes["GET /api/v1/equipment"] = MockResponse().setBody(registerJson)
        val vm = main.track(EquipmentTabsViewModel(EquipmentRepository(api)))
        assertEquals(EquipmentTab.Routers, vm.state.value.tab)
        vm.toggleOnuStatus("faulty")
        assertEquals("faulty", vm.state.value.onuStatus)
        vm.toggleOnuStatus("faulty")
        assertEquals("", vm.state.value.onuStatus)
        vm.toggleOnuOlt("o1")
        assertEquals("o1", vm.state.value.onuOltId)
        vm.toggleOnuOlt("o1")
        assertEquals("", vm.state.value.onuOltId)
        vm.setOnuQuery("zteg")
        vm.setOtherQuery("crs")
        assertEquals("zteg", vm.state.value.onuQuery)
        assertEquals("crs", vm.state.value.otherQuery)
        advanceUntilIdle()
    }

    @Test fun viewModelShowsAFailedRegisterAndKeepsTheTabs() = runTest(main.dispatcher) {
        routes["GET /api/v1/equipment"] = MockResponse().setResponseCode(503).setBody("""{"error":{"code":"db_unavailable","message":"m","details":{}}}""")
        val vm = main.track(EquipmentTabsViewModel(EquipmentRepository(api)))
        await(vm.state) { it.register is Loadable.Failed }
        assertTrue(vm.state.value.other is Loadable.Failed)
        vm.selectTab(EquipmentTab.Olt)
        assertEquals(EquipmentTab.Olt, vm.state.value.tab)
    }
}
