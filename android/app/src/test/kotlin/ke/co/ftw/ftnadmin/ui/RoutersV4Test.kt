package ke.co.ftw.ftnadmin.ui

import ke.co.ftw.ftnadmin.core.network.ApiClient
import ke.co.ftw.ftnadmin.core.network.MemoryTokenStore
import ke.co.ftw.ftnadmin.core.network.Session
import ke.co.ftw.ftnadmin.core.network.SessionManager
import ke.co.ftw.ftnadmin.core.network.UserInfo
import ke.co.ftw.ftnadmin.data.accounts.AccountsRepository
import ke.co.ftw.ftnadmin.data.apartments.ApartmentsRepository
import ke.co.ftw.ftnadmin.data.routers.RouterLiveRepository
import ke.co.ftw.ftnadmin.data.sites.NewDevicesRepository
import ke.co.ftw.ftnadmin.data.sites.SiteSetupRepository
import ke.co.ftw.ftnadmin.data.sites.SitesRepository
import ke.co.ftw.ftnadmin.domain.Account
import ke.co.ftw.ftnadmin.domain.Site
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.customers.add.AddCustomerViewModel
import ke.co.ftw.ftnadmin.ui.customers.add.ProvisionStatus
import ke.co.ftw.ftnadmin.ui.customers.add.provisionStatus
import ke.co.ftw.ftnadmin.ui.customers.macConflictText
import ke.co.ftw.ftnadmin.ui.home.HomeViewModel
import ke.co.ftw.ftnadmin.ui.more.RouterActionsViewModel
import ke.co.ftw.ftnadmin.ui.network.ClientFilter
import ke.co.ftw.ftnadmin.ui.network.NetworkViewModel
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.flow.MutableStateFlow
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
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Rule
import org.junit.Test

/** API v4 view models against a MockWebServer: live dashboard, clients, interfaces, add customer, reboot, aliases. */
@OptIn(ExperimentalCoroutinesApi::class)
class RoutersV4Test {
    @get:Rule val main = MainDispatcherRule()
    private lateinit var server: MockWebServer
    private lateinit var api: ApiClient
    private val routes = mutableMapOf<String, MockResponse>()
    private val sequences = mutableMapOf<String, MutableList<MockResponse>>()
    private val log = mutableListOf<RecordedRequest>()
    private val owner = UserInfo("1", "b", "owner")
    private val operator = UserInfo("2", "o", "operator")

    @Before fun setUp() {
        server = MockWebServer()
        server.dispatcher = object : Dispatcher() {
            override fun dispatch(request: RecordedRequest): MockResponse {
                val key = "${request.method} ${request.requestUrl!!.encodedPath}"
                synchronized(log) { log += request }
                sequences[key]?.let { seq -> synchronized(seq) { return if (seq.size > 1) seq.removeAt(0) else seq[0] } }
                return routes[key] ?: MockResponse().setResponseCode(404).setBody("""{"error":{"code":"not_found","message":"nope"}}""")
            }
        }
        server.start()
        val sessions = SessionManager(MemoryTokenStore(Session("a", "", "r", "", "d", owner)))
        sessions.restore()
        api = ApiClient.create(server.url("/").toString(), sessions, io = main.dispatcher)
    }

    @After fun tearDown() {
        // Cancel the view models first, so no call is resumed on a Main that is about to be reset.
        main.cancelViewModels()
        server.shutdown()
    }

    private fun ok(body: String, code: Int = 200) = MockResponse().setResponseCode(code).setBody(body)

    private fun err(code: Int, c: String, details: String = "{}", msg: String = "m") =
        MockResponse().setResponseCode(code).setBody("""{"error":{"code":"$c","message":"$msg","details":$details}}""")

    private fun <T> TestScope.await(flow: StateFlow<T>, cond: (T) -> Boolean) {
        advanceUntilIdle()
        val end = System.currentTimeMillis() + 5_000
        while (!cond(flow.value) && System.currentTimeMillis() < end) {
            Thread.sleep(20)
            advanceUntilIdle()
        }
        assertTrue("state condition not reached: ${flow.value}", cond(flow.value))
    }

    private fun requests(prefix: String) = synchronized(log) { log.filter { "${it.method} ${it.requestUrl!!.encodedPath}".startsWith(prefix) } }

    // ---- live dashboard ----

    private val live = """{"router_id":"r1","generated_at":"2026-11-18T10:00:00Z","reachable":true,
        "system":{"board":"RB5009","version":"7.16","cpu_count":4,"cpu_load_pct":42.5,"mem_used_bytes":512,"mem_total_bytes":1024,"uptime_s":90061},
        "wan":{"interface":"ether1","running":true,"down_bps":12000000,"up_bps":3000000,"cap_mbps":100,"util_pct":12},
        "tunnel":{"handshake_age_s":4,"rtt_ms":23.4},
        "throughput":{"step_s":300,"t":[],"down_bps":[1,2,3],"up_bps":[1,1,1]},
        "flats":{"total":27,"online":20,"active":22,"expired":3,"suspended":2,"expiring_7d":4},
        "equipment":{"olt":1,"ont":25},"onts_bound":24,"revenue":{"today_kes":1500,"month_kes":42000},"open_alerts":1,"setup_state":"live"}"""

    @Test fun liveDashboardLoadsKeepsHistoryAndSwitchesToFleetOverview() = runTest(main.dispatcher) {
        routes["GET /api/v1/routers/r1/live"] = ok(live)
        routes["GET /api/v1/overview"] = ok("""{"revenue":{"today_kes":900,"spark_7d":[1,2,3]},"active":40,"online_now":31,"routers":{"total":3,"online":2},"health":{"expiring_7d":5}}""")
        val sel = MutableStateFlow<String?>("r1")
        val vm = main.track(HomeViewModel(RouterLiveRepository(api), sel, pollMs = 10))
        await(vm.state) { it.live is Loadable.Ready }

        val l = (vm.state.value.live as Loadable.Ready).value
        assertEquals(42.5, l.system.cpuLoadPct, 0.0)
        assertEquals(50.0, l.system.memPct, 0.0)
        assertEquals(20, l.flats.online)
        assertEquals(listOf(1L, 2L, 3L), l.throughput.downBps)
        assertEquals(12_000_000L, l.wan.downBps)
        assertEquals(1, vm.state.value.cpuHistory.size)

        vm.refresh()
        await(vm.state) { it.cpuHistory.size == 2 }
        assertEquals(listOf(20.0, 20.0), vm.state.value.onlineHistory)

        // "All routers": fleet totals from /overview, with no router filter.
        sel.value = null
        await(vm.state) { it.routerId == null && it.overview is Loadable.Ready }
        val o = (vm.state.value.overview as Loadable.Ready).value
        assertEquals(2, o.routers.online)
        assertEquals(40, o.active)
        assertNull(requests("GET /api/v1/overview").last().requestUrl!!.queryParameter("router"))
    }

    @Test fun liveDashboardPollsWhileResumedAndStopsWhenPaused() = runTest(main.dispatcher) {
        routes["GET /api/v1/routers/r1/live"] = ok(live)
        val vm = main.track(HomeViewModel(RouterLiveRepository(api), MutableStateFlow("r1"), pollMs = 10))
        await(vm.state) { it.live is Loadable.Ready }
        vm.startPolling()
        await(vm.state) { it.cpuHistory.size >= 4 }
        vm.stopPolling()
        advanceUntilIdle()
        Thread.sleep(100)
        advanceUntilIdle()
        val n = requests("GET /api/v1/routers/r1/live").size
        Thread.sleep(100)
        advanceUntilIdle()
        assertEquals("no polling after stop", n, requests("GET /api/v1/routers/r1/live").size)
    }

    @Test fun liveDashboardKeepsOldNumbersWhenARefreshFails() = runTest(main.dispatcher) {
        sequences["GET /api/v1/routers/r1/live"] = mutableListOf(ok(live), err(503, "router_unreachable"))
        val vm = main.track(HomeViewModel(RouterLiveRepository(api), MutableStateFlow("r1")))
        await(vm.state) { it.live is Loadable.Ready }
        vm.refresh()
        await(vm.state) { it.staleError != null }
        assertTrue(vm.state.value.live is Loadable.Ready)
    }

    // ---- clients and interfaces ----

    private val clients = """{"items":[
        {"kind":"flat","account_id":"a1","code":"FTN-1","flat":"A1","tenant":"Jane","mac":"AA:BB:CC:DD:EE:01","vendor":"Huawei","ip":"10.0.0.11","online":true,"lease_status":"bound","usage_today_bytes":1048576},
        {"kind":"unregistered","mac":"AA:BB:CC:DD:EE:09","vendor":"ZTE","host_name":"ont-9","ip":"10.0.0.99","online":true}],
        "counts":{"flats":1,"online":2,"unregistered":1}}"""

    private fun networkVm(sel: StateFlow<String?> = MutableStateFlow("r1")) =
        main.track(NetworkViewModel(RouterLiveRepository(api), AccountsRepository(api), NewDevicesRepository(api), sel, searchDebounceMs = 10))

    @Test fun clientsSearchSendsTheQueryAndFilters() = runTest(main.dispatcher) {
        routes["GET /api/v1/routers/r1/clients"] = ok(clients)
        routes["GET /api/v1/routers/r1/interfaces"] = ok("""{"items":[]}""")
        val vm = networkVm()
        await(vm.state) { it.clients is Loadable.Ready }
        val c = (vm.state.value.clients as Loadable.Ready).value
        assertEquals(2, c.items.size)
        assertEquals(1, c.counts.unregistered)
        assertTrue(c.items[0].isFlat && !c.items[1].isFlat)
        assertEquals("A1", c.items[0].title)
        assertEquals("ont-9", c.items[1].title)

        vm.onQuery("aa:bb:cc:dd:ee:09")
        await(vm.state) { requests("GET /api/v1/routers/r1/clients").any { it.requestUrl!!.queryParameter("q") == "aa:bb:cc:dd:ee:09" } }

        vm.setFilter(ClientFilter.Unregistered)
        await(vm.state) { requests("GET /api/v1/routers/r1/clients").any { it.requestUrl!!.queryParameter("kind") == "unregistered" } }
        vm.setFilter(ClientFilter.Online)
        await(vm.state) { requests("GET /api/v1/routers/r1/clients").any { it.requestUrl!!.queryParameter("online") == "true" } }
    }

    @Test fun clientMatchesNameFlatIpMacVendorAndHost() {
        val c = ke.co.ftw.ftnadmin.data.routers.Client(flat = "B7", tenant = "Otieno", ip = "10.1.2.3", mac = "AA:BB:CC:00:11:22", vendor = "Huawei", hostName = "ont-7")
        listOf("b7", "otieno", "10.1.2", "00:11", "huawei", "ONT-7", "").forEach { assertTrue(it, c.matches(it)) }
        assertFalse(c.matches("zzz"))
    }

    @Test fun interfacesLoadWithNullRatesOnFirstPoll() = runTest(main.dispatcher) {
        routes["GET /api/v1/routers/r1/clients"] = ok("""{"items":[]}""")
        routes["GET /api/v1/routers/r1/interfaces"] = ok(
            """{"items":[{"name":"ether1","type":"ether","running":true,"role":"wan","rx_bps":null,"tx_bps":null,"rx_bytes":100,"tx_bytes":200,"addresses":["192.168.1.2/24"],"mtu":1500},
               {"name":"bridge-cust","type":"bridge","running":false,"disabled":true,"role":"customer","rx_bps":5000,"tx_bps":7000}],"generated_at":"2026-11-18T10:00:00Z"}""",
        )
        val vm = networkVm()
        await(vm.state) { it.interfaces is Loadable.Ready }
        val l = (vm.state.value.interfaces as Loadable.Ready).value
        assertEquals(listOf("ether1", "bridge-cust"), l.map { it.name })
        assertNull(l[0].rxBps)
        assertEquals("wan", l[0].role)
        assertEquals(listOf("192.168.1.2/24"), l[0].addresses)
        assertEquals(7000L, l[1].txBps)
        assertTrue(l[1].disabled)
    }

    @Test fun networkNeedsARouter() = runTest(main.dispatcher) {
        val vm = networkVm(MutableStateFlow(null))
        advanceUntilIdle()
        assertNull(vm.state.value.routerId)
        assertTrue("no router means no calls", requests("GET").isEmpty())
    }

    // ---- add customer ----

    private val pkg = """{"id":"p1","name":"Basic 10","speed_mbps":10,"duration_min":43200,"price_kes":1500}"""
    private val deviceList = """{"items":[{"kind":"unregistered","mac":"AA:BB:CC:DD:EE:09","vendor":"ZTE","host_name":"ont-9","ip":"10.0.0.99","online":true},
        {"kind":"unregistered","mac":"AA:BB:CC:DD:EE:10","vendor":"Huawei","online":false}]}"""

    private fun addRoutes(online: Boolean = true) {
        routes["GET /api/v1/routers"] = ok("""{"items":[{"router_id":"r1","router_name":"Kilimani","online":$online}]}""")
        routes["GET /api/v1/packages"] = ok("""{"items":[$pkg]}""")
        routes["GET /api/v1/apartments"] = ok(
            """{"items":[{"id":"ap1","name":"MG One Central","routers":[{"site_id":"r1","router_id":"r1","router_name":"Kilimani"}]}]}""",
        )
        routes["GET /api/v1/routers/r1/clients"] = ok(deviceList)
    }

    private fun addVm() = main.track(AddCustomerViewModel(
        AccountsRepository(api), RouterLiveRepository(api), SitesRepository(api), ApartmentsRepository(api),
        initialRouterId = "r1", initialMac = null, pollMs = 10, maxPolls = 5,
    ))

    private fun TestScope.fill(vm: AddCustomerViewModel) {
        await(vm.state) { it.routers.isNotEmpty() && it.apartmentId != null && it.packages is Loadable.Ready && it.devices is Loadable.Ready }
        vm.onUnit("A1")
        vm.onTenant("Jane")
        vm.onPackage("p1")
        vm.pickDevice((vm.state.value.devices as Loadable.Ready).value.first())
    }

    private fun accountJson(router: String) =
        """{"id":"a1","code":"FTN-1","router_id":"r1","router_name":"Kilimani","flat":"A1","tenant":"Jane","mac":"AA:BB:CC:DD:EE:09","state":"active",
           "package":{"id":"p1","name":"Basic 10"},"router":$router}"""

    @Test fun addCustomerCreatesAndShowsProvisionedOnRouter() = runTest(main.dispatcher) {
        addRoutes()
        routes["POST /api/v1/accounts"] = ok(
            accountJson("""{"in_sync":true,"pending_task":false,"lease_status":"bound","suspended_on_router":false}"""), code = 201,
        )
        val vm = addVm()
        fill(vm)
        assertEquals("device pick fills the MAC", "AA:BB:CC:DD:EE:09", vm.state.value.macInput)
        assertTrue(vm.state.value.canSubmit)

        vm.submit()
        await(vm.state) { it.added != null }
        assertEquals(ProvisionStatus.Provisioned, vm.state.value.added!!.status)
        assertEquals("Kilimani", vm.state.value.added!!.account.routerLabel)
        val body = requests("POST /api/v1/accounts").single().body.readUtf8()
        assertTrue(body, body.contains("\"router_id\":\"r1\"") && body.contains("\"package_id\":\"p1\"") &&
            body.contains("\"activate\":true") && body.contains("\"mac\":\"AA:BB:CC:DD:EE:09\"") && body.contains("\"unit\":\"A1\"") &&
            body.contains("\"apartment_id\":\"ap1\""))
    }

    @Test fun addCustomerRefusesAnInvalidMacWithoutACall() = runTest(main.dispatcher) {
        addRoutes()
        val vm = addVm()
        fill(vm)
        vm.onMac("AA:BB:CC")
        assertNotNull(vm.state.value.macError)
        assertFalse(vm.state.value.canSubmit)
        vm.submit()
        advanceUntilIdle()
        assertTrue(requests("POST").isEmpty())
        vm.onMac("aa-bb-cc-dd-ee-11")
        assertTrue("pasted spelling is accepted", vm.state.value.canSubmit)
    }

    @Test fun addCustomerNamesTheOwningFlatAndRouterOn409MacInUse() = runTest(main.dispatcher) {
        addRoutes()
        routes["POST /api/v1/accounts"] = err(
            409, "mac_in_use",
            """{"mac":"AA:BB:CC:DD:EE:09","account":{"id":"a2","code":"FTN-2","flat":"C7","router_id":"r2","router_name":"Lavington Court"}}""",
        )
        val vm = addVm()
        fill(vm)
        vm.submit()
        await(vm.state) { !it.submitting && it.macOwner != null }
        val text = macConflictText(vm.state.value.macOwner!!)
        assertTrue(text, text.contains("house C7") && text.contains("Lavington Court") && text.contains("FTN-2"))
        assertNull("shown as its own notice", vm.state.value.error)
        assertNull(vm.state.value.added)
        assertEquals("the form stays editable", "AA:BB:CC:DD:EE:09", vm.state.value.macInput)
    }

    @Test fun addCustomerIsQueuedWhenTheRouterIsOffline() = runTest(main.dispatcher) {
        addRoutes(online = false)
        routes["POST /api/v1/accounts"] = ok(
            accountJson("""{"in_sync":false,"pending_task":true,"lease_status":"none","last_error":""}"""), code = 201,
        )
        val vm = addVm()
        fill(vm)
        vm.submit()
        await(vm.state) { it.added != null }
        assertEquals(ProvisionStatus.Queued, vm.state.value.added!!.status)
        assertTrue(ProvisionStatus.Queued.label.contains("Queued"))
        Thread.sleep(80)
        advanceUntilIdle()
        assertTrue("an offline router is not polled", requests("GET /api/v1/accounts/a1").isEmpty())
    }

    @Test fun addCustomerPollsUntilTheRouterHasIt() = runTest(main.dispatcher) {
        addRoutes()
        routes["POST /api/v1/accounts"] = ok(accountJson("""{"in_sync":false,"pending_task":true}"""), code = 201)
        sequences["GET /api/v1/accounts/a1"] = mutableListOf(
            ok(accountJson("""{"in_sync":false,"pending_task":true}""")),
            ok(accountJson("""{"in_sync":true,"pending_task":false,"lease_status":"bound"}""")),
        )
        val vm = addVm()
        fill(vm)
        vm.submit()
        await(vm.state) { it.added?.status == ProvisionStatus.Provisioned }
        assertTrue(requests("GET /api/v1/accounts/a1").size >= 2)
    }

    @Test fun provisionStatusRules() {
        fun acc(router: String?) = ke.co.ftw.ftnadmin.core.network.ApiJson.decodeFromString(Account.serializer(), """{"id":"a","router":$router}""")
        assertEquals(ProvisionStatus.Working, provisionStatus(acc(null), true))
        assertEquals(ProvisionStatus.Provisioned, provisionStatus(acc("""{"in_sync":true}"""), true))
        assertEquals(ProvisionStatus.Working, provisionStatus(acc("""{"pending_task":true}"""), true))
        assertEquals(ProvisionStatus.Queued, provisionStatus(acc("""{"pending_task":true}"""), false))
        assertTrue(provisionStatus(acc("""{"last_error":"router refused"}"""), true) is ProvisionStatus.Failed)
    }

    // ---- reboot guard ----

    private fun routerList() = MutableStateFlow<Loadable<List<Site>>>(
        Loadable.Ready(listOf(Site(id = "r1", name = "Kilimani", online = true))),
    )

    private fun actionsVm(user: UserInfo) = main.track(RouterActionsViewModel(SiteSetupRepository(api), MutableStateFlow("r1"), routerList(), user))

    @Test fun rebootIsOwnerOnlyAndNeedsTheTypedRouterName() = runTest(main.dispatcher) {
        routes["POST /api/v1/routers/r1/actions/reboot"] = ok("""{"queued":true}""", code = 202)

        val op = actionsVm(operator)
        advanceUntilIdle()
        op.openReboot()
        assertFalse("operators cannot even open the dialog", op.state.value.rebootDialog)
        op.setTypedName("Kilimani")
        op.reboot()
        advanceUntilIdle()
        assertTrue(requests("POST").isEmpty())

        val vm = actionsVm(owner)
        await(vm.state) { it.routerName == "Kilimani" }
        vm.openReboot()
        assertTrue(vm.state.value.rebootDialog)
        vm.setTypedName("kilimani")
        assertFalse(vm.canReboot())
        vm.reboot()
        advanceUntilIdle()
        assertTrue("wrong name sends nothing", requests("POST").isEmpty())

        vm.setTypedName(" Kilimani ")
        assertTrue(vm.canReboot())
        vm.reboot()
        await(vm.state) { it.busy == null && requests("POST").isNotEmpty() && it.message != null }
        val body = requests("POST").single().body.readUtf8()
        assertTrue(body, body.contains("\"confirm_name\":\"Kilimani\""))
        assertFalse(vm.state.value.rebootDialog)
    }

    @Test fun rebootRefusedWhileTheRouterIsBusyExplainsWhy() = runTest(main.dispatcher) {
        routes["POST /api/v1/routers/r1/actions/reboot"] = err(409, "bad_state", msg = "job running")
        val vm = actionsVm(owner)
        await(vm.state) { it.routerName == "Kilimani" }
        vm.openReboot()
        vm.setTypedName("Kilimani")
        vm.reboot()
        await(vm.state) { it.message != null }
        assertTrue(vm.state.value.message!!, vm.state.value.message!!.contains("busy"))
    }

    @Test fun recheckAndResyncUseTheActionEndpoints() = runTest(main.dispatcher) {
        routes["POST /api/v1/routers/r1/actions/recheck"] = ok("""{"site_id":"r1","summary":{"ok":5,"warn":1,"fail":0,"unknown":0},"items":[]}""")
        routes["POST /api/v1/routers/r1/actions/resync"] = ok("""{"queued":true}""", code = 202)
        val vm = actionsVm(operator)
        await(vm.state) { it.routerName == "Kilimani" }
        vm.recheck()
        await(vm.state) { it.message?.contains("5 ok") == true }
        vm.clearMessage()
        vm.resync()
        await(vm.state) { it.message?.contains("Resync queued") == true }
        assertEquals(1, requests("POST /api/v1/routers/r1/actions/recheck").size)
        assertEquals(1, requests("POST /api/v1/routers/r1/actions/resync").size)
    }

    // ---- routers alias fallback ----

    @Test fun routerListReadsRouterFieldsAndFallsBackToTheOlderOnes() = runTest(main.dispatcher) {
        routes["GET /api/v1/routers"] = ok(
            """{"items":[{"router_id":"r1","router_name":"Kilimani","online":true},{"id":"s2","name":"Lavington","online":false},
                {"id":"s3","name":"Old","router_id":"r3","router_name":"New","setup_state":"verifying"}]}""",
        )
        val l = SitesRepository(api).sites()
        assertEquals(listOf("r1", "s2", "r3"), l.map { it.key })
        assertEquals(listOf("Kilimani", "Lavington", "New"), l.map { it.label })
        assertEquals("verifying", l[2].setupState)
        assertEquals("the older alias path is never used", 0, requests("GET /api/v1/sites").size)
    }

    @Test fun accountsAndOwnersFallBackToSiteFieldsWhenRouterFieldsAreAbsent() {
        val json = ke.co.ftw.ftnadmin.core.network.ApiJson
        val v4 = json.decodeFromString(Account.serializer(), """{"id":"a","router_id":"r1","router_name":"Kilimani","site_id":"s1","site_name":"Old"}""")
        val v3 = json.decodeFromString(Account.serializer(), """{"id":"a","site_id":"s1","site_name":"Old"}""")
        assertEquals("r1", v4.routerKey)
        assertEquals("Kilimani", v4.routerLabel)
        assertEquals("s1", v3.routerKey)
        assertEquals("Old", v3.routerLabel)
    }

    @Test fun listCallsUseTheRouterQueryParameter() = runTest(main.dispatcher) {
        routes["GET /api/v1/accounts"] = ok("""{"items":[]}""")
        routes["GET /api/v1/payments"] = ok("""{"items":[]}""")
        AccountsRepository(api).list(siteId = "r1")
        ke.co.ftw.ftnadmin.data.payments.PaymentsRepository(api).list(siteId = "r1")
        assertEquals("r1", requests("GET /api/v1/accounts").single().requestUrl!!.queryParameter("router"))
        assertEquals("r1", requests("GET /api/v1/payments").single().requestUrl!!.queryParameter("router"))
        assertNull(requests("GET /api/v1/accounts").single().requestUrl!!.queryParameter("site"))
    }
}
