package ke.co.ftw.ftnadmin.ui

import ke.co.ftw.ftnadmin.core.network.ApiClient
import ke.co.ftw.ftnadmin.core.network.MemoryTokenStore
import ke.co.ftw.ftnadmin.core.network.Session
import ke.co.ftw.ftnadmin.core.network.SessionManager
import ke.co.ftw.ftnadmin.core.network.UserInfo
import ke.co.ftw.ftnadmin.data.routers.RouterLiveRepository
import ke.co.ftw.ftnadmin.data.sites.SitesRepository
import ke.co.ftw.ftnadmin.ui.more.AddRouterViewModel
import ke.co.ftw.ftnadmin.ui.more.EditRouterViewModel
import ke.co.ftw.ftnadmin.ui.more.RouterField
import ke.co.ftw.ftnadmin.ui.more.RouterForm
import ke.co.ftw.ftnadmin.ui.more.togglePort
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.test.TestScope
import kotlinx.coroutines.test.advanceUntilIdle
import kotlinx.coroutines.test.runTest
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.booleanOrNull
import kotlinx.serialization.json.jsonArray
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
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

@OptIn(ExperimentalCoroutinesApi::class)
class RouterFormTest {
    @get:Rule val main = MainDispatcherRule()
    private lateinit var server: MockWebServer
    private lateinit var api: ApiClient
    private val routes = mutableMapOf<String, MockResponse>()
    private val log = mutableListOf<RecordedRequest>()

    @Before fun setUp() {
        server = MockWebServer()
        server.dispatcher = object : Dispatcher() {
            override fun dispatch(request: RecordedRequest): MockResponse {
                synchronized(log) { log += request }
                return routes["${request.method} ${request.requestUrl!!.encodedPath}"]
                    ?: MockResponse().setResponseCode(404).setBody("""{"error":{"code":"not_found","message":"nope"}}""")
            }
        }
        server.start()
        val sessions = SessionManager(MemoryTokenStore(Session("a", "", "r", "", "d", UserInfo("1", "b", "owner"))))
        sessions.restore()
        api = ApiClient.create(server.url("/").toString(), sessions)
    }

    @After fun tearDown() {
        // Let responses still in flight land while the test Main dispatcher exists, so nothing leaks into the next test.
        Thread.sleep(300)
        main.dispatcher.scheduler.advanceUntilIdle()
        server.shutdown()
    }

    private fun ok(body: String, code: Int = 200) = MockResponse().setResponseCode(code).setBody(body)

    private fun <T> TestScope.await(flow: StateFlow<T>, cond: (T) -> Boolean) {
        advanceUntilIdle()
        val end = System.currentTimeMillis() + 5_000
        while (!cond(flow.value) && System.currentTimeMillis() < end) {
            Thread.sleep(20)
            advanceUntilIdle()
        }
        assertTrue("state condition not reached: ${flow.value}", cond(flow.value))
    }

    private fun bodyOf(method: String, path: String): JsonObject {
        val r = synchronized(log) { log.single { it.method == method && it.requestUrl!!.encodedPath == path } }
        return Json.parseToJsonElement(r.body.readUtf8()).jsonObject
    }

    @Test fun createSendsWanModeAndOnlyFilledFields() = runTest(main.dispatcher) {
        routes["POST /api/v1/routers"] = ok(
            """{"router":{"router_id":"r9","router_name":"Lav"},"join":{"command":"/tool fetch x","expires_at":"2099-01-01T00:00:00Z"}}""",
            201,
        )
        val vm = AddRouterViewModel(RouterLiveRepository(api))
        vm.edit(RouterField.NAME) { it.copy(name = " Lav ", wanMode = "pppoe", pppoeUser = "u1", pppoePass = "s3cret", lanPorts = "ether2,ether3") }
        vm.submit()
        await(vm.state) { it.created != null }
        val b = bodyOf("POST", "/api/v1/routers")
        assertEquals("Lav", b["name"]!!.jsonPrimitive.content)
        assertEquals("pppoe", b["wan_mode"]!!.jsonPrimitive.content)
        assertEquals("u1", b["pppoe_user"]!!.jsonPrimitive.content)
        assertEquals("s3cret", b["pppoe_pass"]!!.jsonPrimitive.content)
        assertEquals("ether2,ether3", b["lan_ports"]!!.jsonPrimitive.content)
        assertFalse("blank fields take the server default", b.containsKey("lan_subnet"))
        assertFalse(b.containsKey("serving_customers"))
        assertEquals("/tool fetch x", vm.state.value.created!!.join.command)
    }

    @Test fun validationFieldsAreShownPerField() = runTest(main.dispatcher) {
        routes["POST /api/v1/routers"] = MockResponse().setResponseCode(400).setBody(
            """{"error":{"code":"validation_failed","message":"invalid","details":{"fields":{"wan_address":"must be IPv4 with prefix","lan_subnet":"overlaps customer_subnet"}}}}""",
        )
        val vm = AddRouterViewModel(RouterLiveRepository(api))
        vm.edit(RouterField.NAME) { it.copy(name = "Lav", wanMode = "static", wanAddress = "bad") }
        vm.submit()
        await(vm.state) { it.fieldErrors.isNotEmpty() }
        val s = vm.state.value
        assertEquals("must be IPv4 with prefix", s.fieldErrors[RouterField.WAN_ADDRESS])
        assertEquals("overlaps customer_subnet", s.fieldErrors[RouterField.LAN_SUBNET])
        assertTrue("the network section opens so the errors are visible", s.advanced)
        assertNull(s.created)
        vm.edit(RouterField.WAN_ADDRESS) { it.copy(wanAddress = "203.0.113.10/29") }
        assertNull("editing a field clears its message", vm.state.value.fieldErrors[RouterField.WAN_ADDRESS])
        assertEquals("overlaps customer_subnet", vm.state.value.fieldErrors[RouterField.LAN_SUBNET])
    }

    @Test fun editNeverShowsThePasswordAndPatchesOnlyWhatIsTyped() = runTest(main.dispatcher) {
        routes["GET /api/v1/routers/r1"] = ok(
            """{"router_id":"r1","router_name":"Kilimani","wan_mode":"pppoe","pppoe_user":"u1","pppoe_pass_set":true,"lan_ports":"ether2","lan_subnet":"192.168.88.0/24"}""",
        )
        routes["PATCH /api/v1/routers/r1"] = ok("""{"router_id":"r1","router_name":"Kilimani"}""")
        var saved = false
        val vm = EditRouterViewModel("r1", SitesRepository(api), RouterLiveRepository(api), onSaved = { saved = true })
        await(vm.state) { it.form != null }
        val f = vm.state.value.form!!
        assertTrue(f.pppoePassSet)
        assertEquals("the stored password is never loaded", "", f.pppoePass)
        assertEquals("pppoe", f.wanMode)

        vm.save()
        await(vm.state) { it.saved }
        assertTrue(saved)
        val b = bodyOf("PATCH", "/api/v1/routers/r1")
        assertFalse("empty password leaves it unchanged", b.containsKey("pppoe_pass"))
        assertFalse(b.containsKey("pppoe_pass_clear"))
        assertEquals("pppoe", b["wan_mode"]!!.jsonPrimitive.content)
    }

    @Test fun clearingThePasswordSendsPppoePassClear() = runTest(main.dispatcher) {
        routes["GET /api/v1/routers/r1"] = ok("""{"router_id":"r1","router_name":"Kilimani","wan_mode":"pppoe","pppoe_pass_set":true}""")
        routes["PATCH /api/v1/routers/r1"] = ok("""{"router_id":"r1"}""")
        val vm = EditRouterViewModel("r1", SitesRepository(api), RouterLiveRepository(api))
        await(vm.state) { it.form != null }
        vm.edit(RouterField.PPPOE_PASS) { it.copy(pppoePassClear = true) }
        vm.save()
        await(vm.state) { it.saved }
        val b = bodyOf("PATCH", "/api/v1/routers/r1")
        assertEquals(true, b["pppoe_pass_clear"]!!.jsonPrimitive.booleanOrNull)
        assertFalse(b.containsKey("pppoe_pass"))
    }

    @Test fun changingTheOltPortOrVlanAsksFirstAndSendsConfirm() = runTest(main.dispatcher) {
        routes["GET /api/v1/routers/r1"] = ok("""{"router_id":"r1","router_name":"Kilimani","olt_interface":"sfp-sfpplus1","vlan_id":100}""")
        routes["PATCH /api/v1/routers/r1"] = ok("""{"router_id":"r1"}""")
        val vm = EditRouterViewModel("r1", SitesRepository(api), RouterLiveRepository(api))
        await(vm.state) { it.form != null }
        // a customer port needs no confirm
        vm.edit(RouterField.CUSTOMER_PORTS) { it.copy(customerPorts = "ether4,ether5") }
        assertFalse(vm.state.value.form!!.changesOlt(vm.state.value.original!!))
        // the OLT port does: nothing is sent until the person confirms
        vm.edit(RouterField.OLT_INTERFACE) { it.copy(oltInterface = "ether3") }
        vm.save()
        assertTrue(vm.state.value.confirmOlt)
        assertTrue(synchronized(log) { log.none { it.method == "PATCH" } })
        vm.cancelConfirm()
        assertFalse(vm.state.value.confirmOlt)
        vm.save()
        vm.confirmSave()
        await(vm.state) { it.saved }
        val b = bodyOf("PATCH", "/api/v1/routers/r1")
        assertEquals(true, b["confirm"]!!.jsonPrimitive.booleanOrNull)
        assertEquals("ether3", b["olt_interface"]!!.jsonPrimitive.content)
    }

    @Test fun customerPortsAreAListNotJustTheBenchAlias() = runTest(main.dispatcher) {
        routes["GET /api/v1/routers/r1"] = ok("""{"router_id":"r1","router_name":"Kilimani","customer_ports":["ether4","ether5"],"bench_port":"ether4","disabled_ports":["ether6"]}""")
        routes["PATCH /api/v1/routers/r1"] = ok("""{"router_id":"r1"}""")
        val vm = EditRouterViewModel("r1", SitesRepository(api), RouterLiveRepository(api))
        await(vm.state) { it.form != null }
        assertEquals("ether4,ether5", vm.state.value.form!!.customerPorts)
        vm.edit(RouterField.CUSTOMER_PORTS) { it.copy(customerPorts = togglePort(it.customerPorts, "ether4")) }
        assertEquals("ether5", vm.state.value.form!!.customerPorts)
        vm.save()
        await(vm.state) { it.saved }
        val b = bodyOf("PATCH", "/api/v1/routers/r1")
        assertEquals(listOf("ether5"), b["customer_ports"]!!.jsonArray.map { it.jsonPrimitive.content })
        assertFalse("the alias is not sent", b.containsKey("bench_port"))
    }

    @Test fun changingTheAdminLanAsksForTheRouterNameAndSendsIt() = runTest(main.dispatcher) {
        routes["GET /api/v1/routers/r1"] = ok("""{"router_id":"r1","router_name":"Kilimani","lan_ports":"ether2"}""")
        routes["PATCH /api/v1/routers/r1"] = ok("""{"router_id":"r1"}""")
        val vm = EditRouterViewModel("r1", SitesRepository(api), RouterLiveRepository(api))
        await(vm.state) { it.form != null }
        vm.edit(RouterField.LAN_PORTS) { it.copy(lanPorts = "ether3") }
        vm.save()
        assertTrue(vm.state.value.confirmLan)
        assertTrue(synchronized(log) { log.none { it.method == "PATCH" } })
        vm.setTypedName("Nairobi")
        assertFalse(vm.canConfirmLan())
        vm.confirmLanSave()
        assertTrue("a wrong name sends nothing", synchronized(log) { log.none { it.method == "PATCH" } })
        vm.setTypedName("kilimani")
        assertFalse("the name must match exactly, case included", vm.canConfirmLan())
        vm.setTypedName("  Kilimani ")
        assertTrue(vm.canConfirmLan())
        vm.confirmLanSave()
        await(vm.state) { it.saved }
        val b = bodyOf("PATCH", "/api/v1/routers/r1")
        assertEquals("Kilimani", b["confirm_name"]!!.jsonPrimitive.content)
        assertEquals("ether3", b["lan_ports"]!!.jsonPrimitive.content)
    }

    @Test fun blankOltPortIsTheDefaultPortAndNoDialogWhenNothingChanges() = runTest(main.dispatcher) {
        routes["GET /api/v1/routers/r1"] = ok("""{"router_id":"r1","router_name":"Kilimani","olt_interface":"","vlan_id":0,"auto_fixes":{"paused":true}}""")
        routes["PATCH /api/v1/routers/r1"] = ok("""{"router_id":"r1"}""")
        val vm = EditRouterViewModel("r1", SitesRepository(api), RouterLiveRepository(api))
        await(vm.state) { it.form != null }
        assertTrue(vm.state.value.autoFixesPaused)
        // typing the default port into a blank field is not a change
        vm.edit(RouterField.OLT_INTERFACE) { it.copy(oltInterface = "sfp-sfpplus1") }
        assertFalse(vm.state.value.form!!.changesOlt(vm.state.value.original!!))
        vm.save()
        assertFalse(vm.state.value.confirmOlt)
    }

    @Test fun switchingAwayFromStaticDropsStaticFields() {
        val i = RouterForm(name = "x", wanMode = "dhcp", wanAddress = "1.2.3.4/24", wanGateway = "1.2.3.1", pppoeUser = "u", pppoePass = "p").toInput()
        assertEquals("", i.wanAddress)
        assertEquals("", i.wanGateway)
        assertEquals("", i.pppoeUser)
        assertEquals("", i.pppoePass)
    }
}
