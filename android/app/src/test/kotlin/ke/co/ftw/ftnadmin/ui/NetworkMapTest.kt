package ke.co.ftw.ftnadmin.ui

import ke.co.ftw.ftnadmin.core.network.ApiClient
import ke.co.ftw.ftnadmin.core.network.ApiJson
import ke.co.ftw.ftnadmin.core.network.MemoryTokenStore
import ke.co.ftw.ftnadmin.core.network.Session
import ke.co.ftw.ftnadmin.core.network.SessionManager
import ke.co.ftw.ftnadmin.core.network.UserInfo
import ke.co.ftw.ftnadmin.data.networkmap.NetApartment
import ke.co.ftw.ftnadmin.data.networkmap.NetHouses
import ke.co.ftw.ftnadmin.data.networkmap.NetOlt
import ke.co.ftw.ftnadmin.data.networkmap.NetRouter
import ke.co.ftw.ftnadmin.data.networkmap.NetworkMap
import ke.co.ftw.ftnadmin.data.networkmap.NetworkMapRepository
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.common.Tone
import ke.co.ftw.ftnadmin.ui.network.MarkerState
import ke.co.ftw.ftnadmin.ui.network.NetworkMapViewModel
import ke.co.ftw.ftnadmin.ui.network.NetworkMode
import ke.co.ftw.ftnadmin.ui.network.alertsText
import ke.co.ftw.ftnadmin.ui.network.argb
import ke.co.ftw.ftnadmin.ui.network.housesText
import ke.co.ftw.ftnadmin.ui.network.listSubtitle
import ke.co.ftw.ftnadmin.ui.network.marker
import ke.co.ftw.ftnadmin.ui.network.markerState
import ke.co.ftw.ftnadmin.ui.network.oltsText
import ke.co.ftw.ftnadmin.ui.network.onusText
import ke.co.ftw.ftnadmin.ui.network.plural
import ke.co.ftw.ftnadmin.ui.network.ponText
import ke.co.ftw.ftnadmin.ui.network.routersText
import ke.co.ftw.ftnadmin.ui.network.sortedForList
import ke.co.ftw.ftnadmin.ui.network.splitByLocation
import ke.co.ftw.ftnadmin.ui.network.stateCounts
import ke.co.ftw.ftnadmin.ui.network.summaryRows
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.test.TestScope
import kotlinx.coroutines.test.advanceUntilIdle
import kotlinx.coroutines.test.runTest
import okhttp3.mockwebserver.Dispatcher
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import okhttp3.mockwebserver.RecordedRequest
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Rule
import org.junit.Test

/** The Network map: marker state, summary wording, the API model and the view model. Every helper used here exists in main. */
@OptIn(ExperimentalCoroutinesApi::class)
class NetworkMapTest {
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

    private val mapJson = """{"with_gps":2,"without_gps":1,"down":1,"partial":0,"ok":1,"apartments":[
      {"id":"a1","name":"Hilltop","address":"Ngong Road","has_gps":true,"lat":-1.2864,"lng":36.8172,"state":"ok",
       "maps_url":"https://www.google.com/maps/search/?api=1&query=-1.2864,36.8172",
       "houses":{"active":8,"total":10,"online":6},
       "routers":[{"id":"r1","name":"Kilimani","state":"up"}],
       "olts":[{"id":"o1","name":"Hilltop OLT","router":"Kilimani","pons":[1,2],"onus_online":3,"onus_total":3}],
       "onus":[{"id":"n1","serial":"ZTEG1","house":"A1","customer":"Wanjiru","online":true,"status":"online","olt":"Hilltop OLT","pon":1,"account_id":"acc1"}],
       "other":[{"id":"s1","kind":"switch","label":"CRS326","status":"installed","house":"A1"}],
       "onus_online":3,"onus_total":3,"open_alerts":0},
      {"id":"a2","name":"Bluegum","address":"","has_gps":true,"lat":0.0,"lng":0.0,"state":"down",
       "houses":{"active":0,"total":4,"online":0},
       "routers":[{"id":"r2","name":"Lavington","state":"down"}],"olts":[],"onus":[],"other":[],"onus_online":0,"onus_total":0,"open_alerts":2},
      {"id":"a3","name":"No Pin Court","address":"Somewhere","has_gps":false,"lat":null,"lng":null,"state":"unknown",
       "houses":{"active":0,"total":0,"online":0},"routers":[],"olts":[],"onus":[],"other":[],"onus_online":0,"onus_total":0,"open_alerts":0}]}"""

    private fun map(): NetworkMap = ApiJson.decodeFromString(NetworkMap.serializer(), mapJson)

    private fun apt(
        id: String = "x",
        name: String = "X",
        routers: List<String> = listOf("up"),
        online: Int = 0,
        total: Int = 0,
        hasGps: Boolean = true,
    ) = NetApartment(
        id = id, name = name, hasGps = hasGps, lat = if (hasGps) -1.0 else null, lng = if (hasGps) 36.0 else null,
        routers = routers.mapIndexed { i, s -> NetRouter("r$i", "R$i", s) }, onusOnline = online, onusTotal = total,
    )

    // ---- marker state ----

    @Test fun markerStateFollowsRoutersAndOnus() {
        assertEquals(MarkerState.Unknown, markerState(emptyList(), 0, 0))
        assertEquals(MarkerState.Ok, markerState(listOf("up"), 3, 3))
        assertEquals(MarkerState.Ok, markerState(listOf("up", "up"), 0, 0))
        assertEquals(MarkerState.Partial, markerState(listOf("up"), 2, 3))
        assertEquals(MarkerState.Down, markerState(listOf("down"), 3, 3))
        assertEquals(MarkerState.Down, markerState(listOf("down", "down"), 0, 0))
        assertEquals(MarkerState.Partial, markerState(listOf("up", "down"), 3, 3))
        assertEquals(MarkerState.Partial, markerState(listOf("paused"), 0, 0))
        assertEquals(MarkerState.Partial, markerState(listOf("waiting"), 0, 0))
        assertEquals(MarkerState.Down, markerState(listOf("down", "paused"), 0, 0))
    }

    @Test fun markerStateOfAnApartmentMatchesTheServersState() {
        for (a in map().apartments) assertEquals(a.name, MarkerState.fromWire(a.state), a.marker)
        assertEquals(MarkerState.Unknown, MarkerState.fromWire("nonsense"))
        assertEquals(listOf("ok", "partial", "down", "unknown"), MarkerState.entries.map { it.wire })
    }

    @Test fun markerWordsToneRankAndColoursAreTheLatticeTealSet() {
        assertEquals(listOf("All good", "Some offline", "Router down", "No router"), MarkerState.entries.map { it.label })
        assertEquals(Tone.Good, MarkerState.Ok.tone)
        assertEquals(Tone.Bad, MarkerState.Down.tone)
        assertTrue(MarkerState.Down.rank > MarkerState.Partial.rank && MarkerState.Partial.rank > MarkerState.Ok.rank && MarkerState.Ok.rank > MarkerState.Unknown.rank)
        assertEquals(0xFF1A6D6BL, MarkerState.Ok.argb()) // Lattice Teal 600
        assertEquals(0xFFC0283DL, MarkerState.Down.argb())
        // no orange (hue ~30) anywhere in the palette
        for (s in MarkerState.entries) {
            val c = s.argb()
            val r = (c shr 16 and 0xFF).toInt()
            val g = (c shr 8 and 0xFF).toInt()
            val b = (c and 0xFF).toInt()
            assertTrue("${s.wire} looks orange", !(r > 200 && g in 100..190 && b < 90))
        }
        assertEquals(4, MarkerState.entries.map { it.argb() }.toSet().size)
    }

    // ---- summary wording ----

    @Test fun summaryFormattingMatchesTheWebCard() {
        val a = map().apartments[0]
        assertEquals("8 active of 10", housesText(a.houses))
        assertEquals("3 of 3 online", onusText(a.onusOnline, a.onusTotal))
        assertEquals("Kilimani (up)", routersText(a.routers))
        assertEquals(" PON 1, 2", ponText(a.olts[0].pons))
        assertEquals("", ponText(emptyList()))
        assertEquals("Hilltop OLT PON 1, 2 (3 of 3 online)", oltsText(a.olts))
        assertEquals("none", oltsText(emptyList()))
        assertEquals("none", routersText(emptyList()))
        assertEquals("No open alerts", alertsText(0))
        assertEquals("1 open alert", alertsText(1))
        assertEquals("2 open alerts", alertsText(2))
        assertEquals("1 house", plural(1, "house"))
        assertEquals("3 houses", plural(3, "house"))
        assertEquals(
            listOf("Address", "Houses", "Online now", "Routers", "OLTs", "ONUs", "Open alerts"),
            summaryRows(a).map { it.first },
        )
        assertEquals("8 active of 10", summaryRows(a)[1].second)
        assertEquals("6", summaryRows(a)[2].second)
        assertEquals("No address", summaryRows(map().apartments[1])[0].second)
        assertEquals("2", summaryRows(map().apartments[1])[6].second)
    }

    @Test fun listSubtitleCountsOfflineOnus() {
        assertEquals("6 of 10 houses online", listSubtitle(map().apartments[0]))
        val a = apt(online = 1, total = 3).copy(houses = NetHouses(active = 1, total = 1, online = 1))
        assertEquals("1 of 1 house online, 2 ONUs offline", listSubtitle(a))
        assertEquals("0 of 0 houses online", listSubtitle(apt()))
    }

    // ---- the model and the lists ----

    @Test fun apiModelDecodesPointsAndEquipment() {
        val m = map()
        assertEquals(3, m.apartments.size)
        assertEquals(2, m.withGps)
        assertEquals(1, m.withoutGps)
        val a = m.apartments[0]
        assertEquals(-1.2864, a.point!!.lat, 0.0)
        assertEquals(36.8172, a.point!!.lng, 0.0)
        assertEquals("acc1", a.onus[0].accountId)
        assertEquals("CRS326", a.other[0].label)
        assertEquals(NetOlt("o1", "Hilltop OLT", "Kilimani", listOf(1, 2), 3, 3), a.olts[0])
        // 0,0 is a real point; only a missing lat or lng means "no point"
        assertNotNull(m.apartments[1].point)
        assertNull(m.apartments[2].point)
    }

    @Test fun splitSortAndCount() {
        val all = map().apartments
        val (with, without) = splitByLocation(all)
        assertEquals(listOf("Hilltop", "Bluegum"), with.map { it.name })
        assertEquals(listOf("No Pin Court"), without.map { it.name })
        // worst first: down, then all good, then no router
        assertEquals(listOf("Bluegum", "Hilltop", "No Pin Court"), sortedForList(all).map { it.name })
        assertEquals(Triple(1, 0, 1), stateCounts(all))
    }

    // ---- the view model ----

    @Test fun viewModelLoadsSelectsAndDismisses() = runTest(main.dispatcher) {
        routes["GET /api/v1/network/map"] = MockResponse().setBody(mapJson)
        val vm = main.track(NetworkMapViewModel(NetworkMapRepository(api)))
        assertEquals(NetworkMode.MapView, vm.state.value.mode)
        await(vm.state) { it.data is Loadable.Ready }
        assertEquals(listOf("Hilltop", "Bluegum"), vm.state.value.located.map { it.name })
        assertEquals(listOf("No Pin Court"), vm.state.value.unlocated.map { it.name })
        assertNull(vm.state.value.selected)
        vm.select("a2")
        assertEquals("Bluegum", vm.state.value.selected?.name)
        vm.dismiss()
        assertNull(vm.state.value.selected)
        vm.select("gone")
        assertNull(vm.state.value.selected) // an id that is not in the data opens nothing
    }

    @Test fun liveViewStaysReachableAndModeChangeClosesTheSheet() = runTest(main.dispatcher) {
        routes["GET /api/v1/network/map"] = MockResponse().setBody(mapJson)
        val vm = main.track(NetworkMapViewModel(NetworkMapRepository(api)))
        await(vm.state) { it.data is Loadable.Ready }
        assertEquals(listOf("Map", "List", "Live"), NetworkMode.entries.map { it.label })
        vm.select("a1")
        vm.setMode(NetworkMode.Live)
        assertEquals(NetworkMode.Live, vm.state.value.mode)
        assertNull(vm.state.value.selected)
        vm.setMode(NetworkMode.ListView)
        assertEquals(NetworkMode.ListView, vm.state.value.mode)
    }

    @Test fun viewModelReportsAFailureThenRecoversOnRetry() = runTest(main.dispatcher) {
        val vm = main.track(NetworkMapViewModel(NetworkMapRepository(api)))
        await(vm.state) { it.data is Loadable.Failed }
        routes["GET /api/v1/network/map"] = MockResponse().setBody(mapJson)
        vm.retry()
        await(vm.state) { it.data is Loadable.Ready }
        assertEquals(3, vm.state.value.apartments.size)
    }
}
