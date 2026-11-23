package ke.co.ftw.ftnadmin.ui

import ke.co.ftw.ftnadmin.core.network.ApiClient
import ke.co.ftw.ftnadmin.core.network.ApiJson
import ke.co.ftw.ftnadmin.core.network.MemoryTokenStore
import ke.co.ftw.ftnadmin.core.network.Session
import ke.co.ftw.ftnadmin.core.network.SessionManager
import ke.co.ftw.ftnadmin.core.network.UserInfo
import ke.co.ftw.ftnadmin.data.accounts.AccountsRepository
import ke.co.ftw.ftnadmin.data.apartments.ApartmentsRepository
import ke.co.ftw.ftnadmin.data.equipment.EquipmentRepository
import ke.co.ftw.ftnadmin.data.equipment.EquipmentTree
import ke.co.ftw.ftnadmin.data.equipment.OltInput
import ke.co.ftw.ftnadmin.data.onu.OnuRepository
import ke.co.ftw.ftnadmin.data.sites.NewDevicesRepository
import ke.co.ftw.ftnadmin.data.sites.SitesRepository
import ke.co.ftw.ftnadmin.domain.NewDevice
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.equipment.EquipmentLevel
import ke.co.ftw.ftnadmin.ui.equipment.EquipmentViewModel
import ke.co.ftw.ftnadmin.ui.equipment.OltFormViewModel
import ke.co.ftw.ftnadmin.ui.equipment.UNPLACED
import ke.co.ftw.ftnadmin.ui.newdevices.NewDevicesViewModel
import ke.co.ftw.ftnadmin.ui.onu.RegisterViewModel
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
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Rule
import org.junit.Test

/** The equipment hierarchy: tree, OLT form, ONU placement, and the New devices classes. Every helper used here is defined here. */
@OptIn(ExperimentalCoroutinesApi::class)
class HierarchyViewModelsTest {
    @get:Rule val main = MainDispatcherRule()
    private lateinit var server: MockWebServer
    private lateinit var api: ApiClient
    private val routes = mutableMapOf<String, MockResponse>()
    private val recorded = mutableListOf<Pair<String, RecordedRequest>>()

    @Before fun setUp() {
        server = MockWebServer()
        server.dispatcher = object : Dispatcher() {
            override fun dispatch(request: RecordedRequest): MockResponse {
                val key = "${request.method} ${request.requestUrl!!.encodedPath}"
                synchronized(recorded) { recorded += key to request }
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

    private fun ok(body: String) = MockResponse().setBody(body)

    private fun err(code: Int, c: String, details: String = "{}", msg: String = "m") =
        MockResponse().setResponseCode(code).setBody("""{"error":{"code":"$c","message":"$msg","details":$details}}""")

    private fun requests(key: String): List<RecordedRequest> =
        synchronized(recorded) { recorded.filter { it.first == key }.map { it.second } }

    private fun <T> TestScope.await(flow: StateFlow<T>, cond: (T) -> Boolean) {
        advanceUntilIdle()
        val end = System.currentTimeMillis() + 5_000
        while (!cond(flow.value) && System.currentTimeMillis() < end) {
            Thread.sleep(20)
            advanceUntilIdle()
        }
        assertTrue("state condition not reached: ${flow.value}", cond(flow.value))
    }

    private fun repo() = EquipmentRepository(api)

    private val onuJson = """{"id":"n1","serial":"ZTEG1","model":"F670L","mac":"AA:BB:CC:00:00:01","status":"installed","olt_id":"o1","pon_port":2,
        "house":{"account_id":"a1","code":"FTN-1","unit":"A1","apartment_id":"ap1","apartment":"Hilltop","customer":"Wanjiru"},
        "online":true,"last_seen":"2026-11-20T10:00:00Z","tr069":{"linked":true,"last_inform":"2026-11-20T10:00:00Z","held_back":false},"warnings":[]}"""
    private val looseOnu = """{"id":"n2","serial":"ZTEG2","mac":"AA:BB:CC:00:00:02","status":"in_stock","house":null,"online":false,"last_seen":null,"tr069":null,"warnings":[]}"""
    private val oltJson = """{"id":"o1","name":"Hilltop OLT","model":"V1600G","mac":"AA:BB:CC:00:0F:01","status":"installed","router_id":"r1","router_port":"ether5",
        "pon_ports":4,"has_mgmt_pass":true,"onu_count":1,"online_count":1,"warning_count":0,
        "pons":[{"port":1,"apartments":[],"onus":[]},{"port":2,"apartments":[{"id":"ap1","name":"Hilltop"}],"onus":[$onuJson]}]}"""
    private val treeJson = """{"routers":[{"id":"r1","name":"Kilimani","apartments":[{"id":"ap1","name":"Hilltop"}],"olts":[$oltJson],"onu_count":1,"online_count":1}],
        "unplaced_olts":[],"unplaced_onus":[$looseOnu]}"""

    // ---- the tree ----

    @Test fun treeDecodesAndDrillsDownFromRouterToOnu() = runTest(main.dispatcher) {
        routes["GET /api/v1/equipment/tree"] = ok(treeJson)
        val vm = main.track(EquipmentViewModel(repo()))
        await(vm.state) { it.tree is Loadable.Ready }
        assertEquals(EquipmentLevel.Root, vm.state.value.level)
        vm.openRouter("r1")
        assertEquals("Kilimani", vm.state.value.title)
        assertEquals(listOf("Hilltop OLT"), vm.state.value.olts.map { it.name })
        vm.openOlt("o1")
        assertEquals(EquipmentLevel.Olt, vm.state.value.level)
        assertTrue("the stored password is only a flag", vm.state.value.olt!!.hasMgmtPass)
        vm.openPon(2)
        assertEquals("PON port 2", vm.state.value.title)
        val onu = vm.state.value.pon!!.onus.single()
        assertEquals("A1", onu.house!!.unit)
        assertTrue(onu.tr069!!.linked)
        assertEquals("Serves: Hilltop", "Serves: " + vm.state.value.pon!!.apartmentText)
        // back climbs one level at a time and finally reports the root
        assertTrue(vm.up())
        assertEquals(EquipmentLevel.Olt, vm.state.value.level)
        assertTrue(vm.up())
        assertTrue(vm.up())
        assertEquals(EquipmentLevel.Root, vm.state.value.level)
        assertFalse(vm.up())
    }

    @Test fun unplacedOnusAreListedUnderTheNotPlacedNode() = runTest(main.dispatcher) {
        routes["GET /api/v1/equipment/tree"] = ok(treeJson)
        val vm = main.track(EquipmentViewModel(repo()))
        await(vm.state) { it.tree is Loadable.Ready }
        vm.openRouter(UNPLACED)
        assertEquals("Not placed", vm.state.value.title)
        assertEquals(listOf("ZTEG2"), vm.state.value.unplacedOnus.map { it.title })
        assertNull(vm.state.value.unplacedOnus.single().house)
    }

    @Test fun treeJsonToleratesMissingFields() {
        val t = ApiJson.decodeFromString(EquipmentTree.serializer(), """{"routers":[{"id":"r9","name":"X"}]}""")
        assertEquals(0, t.routers.single().olts.size)
        assertTrue(t.unplacedOnus.isEmpty())
    }

    @Test fun treeFailureIsShown() = runTest(main.dispatcher) {
        routes["GET /api/v1/equipment/tree"] = err(503, "db_unavailable")
        val vm = main.track(EquipmentViewModel(repo()))
        await(vm.state) { it.tree is Loadable.Failed }
    }

    // ---- placing an ONU ----

    @Test fun placingNeedsAPortAndSendsTheOltAndPort() = runTest(main.dispatcher) {
        routes["GET /api/v1/equipment/tree"] = ok(treeJson)
        routes["PUT /api/v1/equipment/n2/placement"] = ok("""{"id":"n2","serial":"ZTEG2","olt_id":"o1","olt_name":"Hilltop OLT","pon_port":2,"warnings":[]}""")
        val vm = main.track(EquipmentViewModel(repo()))
        await(vm.state) { it.tree is Loadable.Ready }
        vm.startMove(vm.state.value.unplacedOnus.single())
        vm.setMoveOlt("o1")
        vm.applyMove()
        assertNotNull("no port: refused locally", vm.state.value.move!!.error)
        assertTrue(requests("PUT /api/v1/equipment/n2/placement").isEmpty())
        vm.setMovePort("2")
        vm.applyMove()
        await(vm.state) { it.move == null && it.info != null }
        val body = requests("PUT /api/v1/equipment/n2/placement").single().body.readUtf8()
        assertTrue(body, body.contains("\"olt_id\":\"o1\"") && body.contains("\"pon_port\":2"))
        assertEquals("ONU placed on Hilltop OLT, PON 2", vm.state.value.info)
    }

    @Test fun aWarningFromTheServerIsShownButTheChangeIsKept() = runTest(main.dispatcher) {
        routes["GET /api/v1/equipment/tree"] = ok(treeJson)
        routes["PUT /api/v1/equipment/n1/placement"] =
            ok("""{"id":"n1","serial":"ZTEG1","olt_id":"o1","olt_name":"Hilltop OLT","pon_port":1,"warnings":["PON 1 does not serve Hilltop"]}""")
        val vm = main.track(EquipmentViewModel(repo()))
        await(vm.state) { it.tree is Loadable.Ready }
        vm.openRouter("r1")
        vm.openOlt("o1")
        vm.openPon(2)
        vm.startMove(vm.state.value.pon!!.onus.single())
        assertEquals("2", vm.state.value.move!!.port)
        vm.setMovePort("1")
        vm.applyMove()
        await(vm.state) { it.move == null }
        assertEquals("PON 1 does not serve Hilltop", vm.state.value.info)
    }

    @Test fun takingAnOnuOutSendsABlankOlt() = runTest(main.dispatcher) {
        routes["GET /api/v1/equipment/tree"] = ok(treeJson)
        routes["PUT /api/v1/equipment/n1/placement"] = ok("""{"id":"n1","serial":"ZTEG1","warnings":[]}""")
        val vm = main.track(EquipmentViewModel(repo()))
        await(vm.state) { it.tree is Loadable.Ready }
        vm.openRouter("r1")
        vm.openOlt("o1")
        vm.openPon(2)
        vm.startMove(vm.state.value.pon!!.onus.single())
        vm.setMoveOlt("o1") // a second tap clears the choice
        vm.applyMove()
        await(vm.state) { it.move == null }
        val body = requests("PUT /api/v1/equipment/n1/placement").single().body.readUtf8()
        assertTrue(body, body.contains("\"olt_id\":\"\"") && body.contains("\"pon_port\":0"))
    }

    // ---- the OLT form ----

    private fun formVm(id: String? = null, site: String? = null) =
        main.track(OltFormViewModel(id, repo(), SitesRepository(api), ApartmentsRepository(api), site))

    private val sitesJson = """{"items":[{"id":"r1","name":"Kilimani","router_id":"r1","router_name":"Kilimani"}]}"""
    private val aptsJson = """{"items":[{"id":"ap1","name":"Hilltop"},{"id":"ap2","name":"Lavington Court"}]}"""

    @Test fun oltFormRefusesAMissingMacAndRouterWithoutACall() = runTest(main.dispatcher) {
        routes["GET /api/v1/routers"] = ok(sitesJson)
        val vm = formVm()
        await(vm.state) { !it.loading && it.routers.isNotEmpty() }
        vm.onName("Hilltop OLT")
        vm.save()
        val e = vm.state.value.form.errors
        assertNotNull(e["mac"])
        assertNotNull(e["site_id"])
        assertTrue(requests("POST /api/v1/olts").isEmpty())
    }

    @Test fun oltFormSendsMacPortsApartmentsAndThePasswordOnce() = runTest(main.dispatcher) {
        routes["GET /api/v1/routers"] = ok(sitesJson)
        routes["GET /api/v1/apartments"] = ok(aptsJson)
        routes["POST /api/v1/olts"] = ok("""{"id":"o9","name":"Hilltop OLT","mac":"AA:BB:CC:00:0F:01","has_mgmt_pass":true}""")
        val vm = formVm(site = "r1")
        await(vm.state) { it.apartments.size == 2 }
        vm.onName("Hilltop OLT")
        vm.onMac("aa-bb-cc-00-0f-01")
        vm.onPonPorts("4")
        vm.toggleApartment(2, "ap1")
        vm.toggleApartment(2, "ap2")
        vm.toggleApartment(2, "ap2") // unticked again
        vm.onMgmtIp("10.0.0.9")
        vm.onMgmtUser("admin")
        vm.onMgmtPass("s3cret")
        vm.save()
        await(vm.state) { it.saved != null }
        val body = requests("POST /api/v1/olts").single().body.readUtf8()
        assertTrue(body, body.contains("\"mac\":\"AA:BB:CC:00:0F:01\"") && body.contains("\"pon_ports\":4"))
        assertTrue(body, body.contains("\"pon_apartments\":{\"2\":[\"ap1\"]}"))
        assertTrue(body, body.contains("\"mgmt_pass\":\"s3cret\""))
        assertEquals("the typed password is dropped from the form after saving", "", vm.state.value.form.mgmtPass)
    }

    @Test fun editingAnOltNeverFillsThePasswordAndABlankOneIsNotSent() = runTest(main.dispatcher) {
        routes["GET /api/v1/routers"] = ok(sitesJson)
        routes["GET /api/v1/apartments"] = ok(aptsJson)
        routes["GET /api/v1/olts/o1"] = ok(
            """{"id":"o1","name":"Hilltop OLT","mac":"AA:BB:CC:00:0F:01","site_id":"r1","pon_ports":4,"pon_apartments":{"2":["ap1"]},"mgmt_user":"admin","has_mgmt_pass":true}""",
        )
        routes["PUT /api/v1/olts/o1"] = ok("""{"id":"o1","name":"Hilltop OLT 2","mac":"AA:BB:CC:00:0F:01","has_mgmt_pass":true}""")
        val vm = formVm("o1")
        await(vm.state) { !it.loading && it.form.name.isNotEmpty() }
        assertTrue(vm.state.value.hadPass)
        assertEquals("", vm.state.value.form.mgmtPass)
        assertEquals(setOf("ap1"), vm.state.value.form.ponApartments[2])
        vm.onName("Hilltop OLT 2")
        vm.save()
        await(vm.state) { it.saved != null }
        val body = requests("PUT /api/v1/olts/o1").single().body.readUtf8()
        assertFalse(body, body.contains("mgmt_pass"))
        assertTrue(body, body.contains("\"pon_apartments\":{\"2\":[\"ap1\"]}"))
    }

    @Test fun aMacOfAnotherDeviceShowsOnTheMacField() = runTest(main.dispatcher) {
        routes["GET /api/v1/routers"] = ok(sitesJson)
        routes["POST /api/v1/olts"] = err(400, "validation_failed", """{"fields":{"mac":"that MAC belongs to a customer account"}}""")
        val vm = formVm(site = "r1")
        await(vm.state) { !it.loading }
        vm.onName("X")
        vm.onMac("AA:BB:CC:00:0F:01")
        vm.save()
        await(vm.state) { !it.busy && it.form.errors["mac"] != null }
        assertNull(vm.state.value.saved)
    }

    @Test fun requestBodyOmitsAnUntouchedMappingAndAnEmptyPassword() {
        val body = EquipmentRepository.oltBody(OltInput(name = "A", mac = "AA:BB:CC:00:0F:01", siteId = "r1")).toString()
        assertFalse(body, body.contains("pon_apartments"))
        assertFalse(body, body.contains("mgmt_pass"))
        val cleared = EquipmentRepository.oltBody(OltInput(name = "A", mac = "AA:BB:CC:00:0F:01", siteId = "r1", clearMgmtPass = true)).toString()
        assertTrue(cleared, cleared.contains("\"clear_mgmt_pass\":true"))
    }

    // ---- the ONU register ----

    @Test fun anOnuAddedToTheRegisterCanNameItsOltAndPort() = runTest(main.dispatcher) {
        routes["GET /api/v1/equipment"] = ok("""{"items":[]}""")
        routes["GET /api/v1/olts"] = ok("""{"items":[{"id":"o1","name":"Hilltop OLT","mac":"AA:BB:CC:00:0F:01"}]}""")
        routes["POST /api/v1/equipment"] = ok("""{"id":"n5","kind":"ont","serial":"S1","mac":"AA:BB:CC:00:00:05","olt_id":"o1","olt_name":"Hilltop OLT","pon_port":3}""")
        val vm = main.track(RegisterViewModel(OnuRepository(api), repo()))
        await(vm.state) { it.items is Loadable.Ready }
        vm.openAdd()
        await(vm.state) { it.olts.isNotEmpty() }
        vm.onSerial("S1")
        vm.onMac("AA:BB:CC:00:00:05")
        vm.onOlt("o1")
        vm.save()
        assertNotNull("an OLT needs its port", vm.state.value.form.errors["pon_port"])
        assertTrue(requests("POST /api/v1/equipment").isEmpty())
        vm.onPonPort("3")
        vm.save()
        await(vm.state) { !it.adding }
        val body = requests("POST /api/v1/equipment").single().body.readUtf8()
        assertTrue(body, body.contains("\"olt_id\":\"o1\"") && body.contains("\"pon_port\":3"))
    }

    // ---- New devices ----

    private fun devicesVm() =
        main.track(NewDevicesViewModel(null, NewDevicesRepository(api), SitesRepository(api), AccountsRepository(api), ApartmentsRepository(api)))

    private fun device(mac: String, cls: String) =
        """{"site_id":"r1","router_id":"r1","router_name":"Kilimani","mac":"$mac","online":true,"class":$cls}"""

    private fun openFirst(vm: NewDevicesViewModel) {
        vm.open((vm.state.value.devices as Loadable.Ready<List<NewDevice>>).value.single())
    }

    @Test fun anOltRowIsDismissOnlyAndNeverCreatesACustomer() = runTest(main.dispatcher) {
        routes["GET /api/v1/unregistered"] = ok(
            """{"items":[${device("AA:BB:CC:00:0F:01", """{"kind":"olt","label":"OLT: Hilltop OLT","creatable":false,"attachable":false,"never_asked":false,"apartment_ids":[]}""")}]}""",
        )
        routes["GET /api/v1/packages"] = ok("""{"items":[]}""")
        routes["GET /api/v1/apartments"] = ok(aptsJson)
        val vm = devicesVm()
        await(vm.state) { it.devices is Loadable.Ready }
        openFirst(vm)
        assertTrue(vm.state.value.sheet!!.device.deviceClass.isOlt)
        vm.setFlat("A1")
        vm.setApartment("ap1")
        vm.create()
        advanceUntilIdle()
        assertTrue(requests("POST /api/v1/unregistered/create").isEmpty())
    }

    @Test fun anOnuRowCreatesWithItsEquipmentAndPreselectsTheApartmentItsPortServes() = runTest(main.dispatcher) {
        routes["GET /api/v1/unregistered"] = ok(
            """{"items":[${device("AA:BB:CC:00:00:01", """{"kind":"onu","label":"ONU ZTEG1","equipment_id":"n1","creatable":true,"attachable":true,"never_asked":false,"apartment_ids":["ap2"]}""")}]}""",
        )
        routes["GET /api/v1/packages"] = ok("""{"items":[]}""")
        routes["GET /api/v1/apartments"] = ok(aptsJson)
        routes["POST /api/v1/unregistered/create"] = ok("""{"id":"a1","flat":"B1","unit":"B1"}""")
        val vm = devicesVm()
        await(vm.state) { it.devices is Loadable.Ready }
        openFirst(vm)
        await(vm.state) { it.sheet?.apartmentId != null }
        assertEquals("the router has two apartments; the port serves one", "ap2", vm.state.value.sheet!!.apartmentId)
        vm.setFlat("B1")
        vm.create()
        await(vm.state) { it.sheet == null && it.message != null }
        val body = requests("POST /api/v1/unregistered/create").single().body.readUtf8()
        assertTrue(body, body.contains("\"equipment_id\":\"n1\"") && body.contains("\"apartment_id\":\"ap2\""))
        assertFalse(body, body.contains("confirm_no_address"))
    }

    @Test fun aBridgeOnlyDeviceAsksForConfirmationThenRetriesWithIt() = runTest(main.dispatcher) {
        routes["GET /api/v1/unregistered"] = ok(
            """{"items":[${device("AA:BB:CC:00:00:09", """{"kind":"unknown","label":"","equipment_id":"","creatable":true,"attachable":true,"never_asked":true,"apartment_ids":[]}""")}]}""",
        )
        routes["GET /api/v1/packages"] = ok("""{"items":[]}""")
        routes["GET /api/v1/apartments"] = ok("""{"items":[{"id":"ap1","name":"Hilltop"}]}""")
        routes["POST /api/v1/unregistered/create"] = err(409, "confirm_required", msg = "this device never asked for an address")
        val vm = devicesVm()
        await(vm.state) { it.devices is Loadable.Ready }
        openFirst(vm)
        await(vm.state) { it.sheet?.apartmentId == "ap1" }
        vm.setFlat("C3")
        vm.create()
        await(vm.state) { it.sheet?.confirmNoAddress == true }
        assertNull("the prompt is not a generic error", vm.state.value.sheet!!.error)
        routes["POST /api/v1/unregistered/create"] = ok("""{"id":"a2","flat":"C3","unit":"C3"}""")
        vm.confirmCreateWithoutAddress()
        await(vm.state) { it.sheet == null && it.message != null }
        val posts = requests("POST /api/v1/unregistered/create")
        assertEquals(2, posts.size)
        assertFalse(posts[0].body.readUtf8().contains("confirm_no_address"))
        assertTrue(posts[1].body.readUtf8().contains("\"confirm_no_address\":true"))
    }

    @Test fun deviceWithoutAClassFieldIsUnknownAndCreatable() {
        val d = ApiJson.decodeFromString(NewDevice.serializer(), """{"mac":"AA:BB:CC:00:00:77"}""")
        assertEquals("unknown", d.deviceClass.kind)
        assertTrue(d.deviceClass.creatable)
        assertFalse(d.deviceClass.isOlt)
    }
}
