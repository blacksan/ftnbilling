package ke.co.ftw.ftnadmin.ui

import ke.co.ftw.ftnadmin.core.network.ApiClient
import ke.co.ftw.ftnadmin.core.network.ApiJson
import ke.co.ftw.ftnadmin.core.network.MemoryTokenStore
import ke.co.ftw.ftnadmin.core.network.Session
import ke.co.ftw.ftnadmin.core.network.SessionManager
import ke.co.ftw.ftnadmin.core.network.UserInfo
import ke.co.ftw.ftnadmin.data.accounts.AccountsRepository
import ke.co.ftw.ftnadmin.data.apartments.ApartmentsRepository
import ke.co.ftw.ftnadmin.data.routers.RouterLiveRepository
import ke.co.ftw.ftnadmin.data.sites.NewDevicesRepository
import ke.co.ftw.ftnadmin.data.sites.SitesRepository
import ke.co.ftw.ftnadmin.domain.Account
import ke.co.ftw.ftnadmin.domain.Apartment
import ke.co.ftw.ftnadmin.domain.ApartmentContact
import ke.co.ftw.ftnadmin.domain.GpsPoint
import ke.co.ftw.ftnadmin.domain.NewDevice
import ke.co.ftw.ftnadmin.ui.apartments.ApartmentDetailViewModel
import ke.co.ftw.ftnadmin.ui.apartments.ApartmentFormViewModel
import ke.co.ftw.ftnadmin.ui.apartments.ApartmentsViewModel
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.customers.CustomerDetailViewModel
import ke.co.ftw.ftnadmin.ui.customers.add.AddCustomerViewModel
import ke.co.ftw.ftnadmin.ui.newdevices.NewDevicesViewModel
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

/** Apartments (API v6) against a MockWebServer: list, detail, form, move, add customer, New devices. */
@OptIn(ExperimentalCoroutinesApi::class)
class ApartmentsTest {
    @get:Rule val main = MainDispatcherRule()
    private lateinit var server: MockWebServer
    private lateinit var api: ApiClient
    private val routes = mutableMapOf<String, MockResponse>()
    private val log = mutableListOf<RecordedRequest>()

    @Before fun setUp() {
        server = MockWebServer()
        server.dispatcher = object : Dispatcher() {
            override fun dispatch(request: RecordedRequest): MockResponse {
                val key = "${request.method} ${request.requestUrl!!.encodedPath}"
                synchronized(log) { log += request }
                return routes[key] ?: MockResponse().setResponseCode(404).setBody("""{"error":{"code":"not_found","message":"nope"}}""")
            }
        }
        server.start()
        val sessions = SessionManager(MemoryTokenStore(Session("a", "", "r", "", "d", UserInfo("1", "b", "owner"))))
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

    private val apt = """{"id":"ap1","name":"MG One Central","address":"Ngong Road","notes":"",
        "caretaker":{"name":"Mzee Otieno","phone":"+254722000111","phone2":"","email":"","notes":"gate","whatsapp_url":"https://wa.me/254722000111"},
        "landlord":{"name":"","phone":"","phone2":"","email":"","notes":"","whatsapp_url":""},
        "routers":[{"site_id":"r1","site_name":"Kilimani","router_id":"r1","router_name":"Kilimani","accounts":2}],
        "accounts_total":3,"accounts_active":2,"online_now":1,
        "accounts":[{"id":"a1","code":"FTN-1","flat":"B4","unit":"B4","apartment":{"id":"ap1","name":"MG One Central"},"state":"active"}]}"""

    // ---- models ----

    @Test fun apartmentJsonKeepsContactsRoutersAndAccounts() {
        val a = ApiJson.decodeFromString(Apartment.serializer(), apt)
        assertEquals("MG One Central", a.name)
        assertEquals("tel:+254722000111", a.caretaker.telUri)
        assertEquals("https://wa.me/254722000111", a.caretaker.whatsappUrl)
        assertTrue("an empty section is empty", a.landlord.isEmpty)
        assertNull(a.landlord.telUri)
        assertEquals("r1", a.routers.single().key)
        assertEquals("B4", a.accounts.single().unitLabel)
        assertEquals("MG One Central", a.accounts.single().apartment?.name)
    }

    @Test fun accountShowsApartmentUnitAndProblemInWords() {
        val acc = ApiJson.decodeFromString(
            Account.serializer(),
            """{"id":"a","flat":"B4","unit":"B4","apartment":{"id":"ap1","name":"Block A"},"problem":"its router does not serve this apartment"}""",
        )
        assertEquals("Block A", acc.apartment?.name)
        assertEquals("its router does not serve this apartment", acc.problem)
        val old = ApiJson.decodeFromString(Account.serializer(), """{"id":"a","flat":"C1"}""")
        assertNull("an account with no apartment decodes", old.apartment)
        assertEquals("the unit falls back to the flat label", "C1", old.unitLabel)
    }

    // ---- list, detail, form ----

    @Test fun listLoadsAndSearchAsksTheServer() = runTest(main.dispatcher) {
        routes["GET /api/v1/apartments"] = ok("""{"items":[$apt],"next_cursor":null}""")
        val vm = main.track(ApartmentsViewModel(ApartmentsRepository(api), debounceMs = 0))
        await(vm.state) { it.items is Loadable.Ready }
        assertEquals("ap1", (vm.state.value.items as Loadable.Ready).value.single().id)
        vm.onQuery("otieno")
        await(vm.state) { requests("GET /api/v1/apartments").size >= 2 }
        val last = requests("GET /api/v1/apartments").last().requestUrl!!
        assertEquals("otieno", last.queryParameter("q"))
    }

    @Test fun detailLinksAndUnlinksRouters() = runTest(main.dispatcher) {
        routes["GET /api/v1/apartments/ap1"] = ok(apt)
        routes["GET /api/v1/routers"] = ok("""{"items":[{"id":"r1","name":"Kilimani"},{"id":"r2","name":"Lavington"}]}""")
        routes["POST /api/v1/apartments/ap1/routers"] = ok(apt.replace("\"routers\":[", "\"routers\":[{\"site_id\":\"r2\",\"router_id\":\"r2\",\"router_name\":\"Lavington\"},"))
        val vm = main.track(ApartmentDetailViewModel("ap1", ApartmentsRepository(api), SitesRepository(api)))
        await(vm.state) { it.apartment is Loadable.Ready && it.allRouters.isNotEmpty() }
        assertEquals("only the router that does not serve it yet is offered", listOf("r2"), vm.state.value.linkable.map { it.key })
        vm.link("r2")
        await(vm.state) { it.message == "Router linked" }
        assertTrue(requests("POST /api/v1/apartments/ap1/routers").single().body.readUtf8().contains("\"site_id\":\"r2\""))
        assertEquals(2, (vm.state.value.apartment as Loadable.Ready).value.routers.size)
    }

    @Test fun unlinkRefusedWhileFlatsAreOnTheRouterSaysSoInWords() = runTest(main.dispatcher) {
        routes["GET /api/v1/apartments/ap1"] = ok(apt)
        routes["GET /api/v1/routers"] = ok("""{"items":[]}""")
        routes["DELETE /api/v1/apartments/ap1/routers/r1"] = err(409, "router_serves_accounts", msg = "Kilimani still serves 2 flats of MG One Central.")
        val vm = main.track(ApartmentDetailViewModel("ap1", ApartmentsRepository(api), SitesRepository(api)))
        await(vm.state) { it.apartment is Loadable.Ready }
        vm.askUnlink("r1")
        vm.unlink()
        await(vm.state) { it.message != null }
        val m = vm.state.value.message!!
        assertTrue(m, m.contains("still serves 2 flats") && m.contains("Move those customers"))
        assertEquals("the apartment still lists the router", 1, (vm.state.value.apartment as Loadable.Ready).value.routers.size)
    }

    @Test fun formCreatesWithRoutersAndContactsAndShowsPhoneErrors() = runTest(main.dispatcher) {
        routes["GET /api/v1/routers"] = ok("""{"items":[{"id":"r1","name":"Kilimani"}]}""")
        routes["POST /api/v1/apartments"] = err(400, "validation_failed", """{"fields":{"caretaker.phone":"Caretaker phone: not a Kenyan number"}}""")
        val vm = main.track(ApartmentFormViewModel(null, ApartmentsRepository(api), SitesRepository(api)))
        await(vm.state) { it.allRouters.isNotEmpty() }
        assertEquals("a single router is preselected", setOf("r1"), vm.state.value.routerIds)
        vm.onName("MG One Central")
        vm.onCaretaker(ApartmentContact(name = "Mzee Otieno", phone = "12"))
        vm.save()
        await(vm.state) { !it.saving && it.fieldErrors.isNotEmpty() }
        assertEquals("Caretaker phone: not a Kenyan number", vm.state.value.fieldErrors["caretaker.phone"])
        val body = requests("POST /api/v1/apartments").single().body.readUtf8()
        assertTrue(body, body.contains("\"router_ids\":[\"r1\"]") && body.contains("\"caretaker\":{") && body.contains("\"name\":\"MG One Central\""))
        routes["POST /api/v1/apartments"] = ok(apt, code = 201)
        vm.onCaretaker(ApartmentContact(name = "Mzee Otieno", phone = "0722000111"))
        vm.save()
        await(vm.state) { it.savedId != null }
        assertEquals("ap1", vm.state.value.savedId)
    }

    // ---- move a customer ----

    @Test fun moveSendsApartmentRouterAndUnitAndUnitInUseIsPlain() = runTest(main.dispatcher) {
        routes["GET /api/v1/accounts/a1"] = ok("""{"id":"a1","flat":"B4","unit":"B4","apartment":{"id":"ap1","name":"Old"},"state":"active"}""")
        routes["GET /api/v1/packages"] = ok("""{"items":[]}""")
        routes["GET /api/v1/apartments"] = ok("""{"items":[$apt]}""")
        routes["POST /api/v1/accounts/a1/move"] = err(409, "unit_in_use", msg = "Unit C2 is used.")
        val vm = main.track(CustomerDetailViewModel("a1", AccountsRepository(api), ApartmentsRepository(api)))
        await(vm.state) { it.account is Loadable.Ready }
        vm.openMove()
        await(vm.state) { it.apartments is Loadable.Ready }
        vm.move("ap1", "r1", "C2")
        await(vm.state) { !it.busy && it.error != null }
        assertTrue(vm.state.value.error!!.contains("already used"))
        val body = requests("POST /api/v1/accounts/a1/move").single().body.readUtf8()
        assertTrue(body, body.contains("\"apartment_id\":\"ap1\"") && body.contains("\"site_id\":\"r1\"") && body.contains("\"unit\":\"C2\""))
        routes["POST /api/v1/accounts/a1/move"] = ok("""{"id":"a1","flat":"C2","unit":"C2","apartment":{"id":"ap1","name":"MG One Central"},"state":"active"}""")
        vm.move("ap1", null, "C2")
        await(vm.state) { it.info != null }
        assertEquals("C2", (vm.state.value.account as Loadable.Ready).value.unitLabel)
    }

    // ---- add customer ----

    private fun addRoutes(apartments: String) {
        routes["GET /api/v1/routers"] = ok("""{"items":[{"router_id":"r1","router_name":"Kilimani","online":true},{"router_id":"r2","router_name":"Lavington","online":true}]}""")
        routes["GET /api/v1/packages"] = ok("""{"items":[{"id":"p1","name":"Basic 10","speed_mbps":10,"duration_min":43200,"price_kes":1500}]}""")
        routes["GET /api/v1/apartments"] = ok(apartments)
        routes["GET /api/v1/routers/r1/clients"] = ok("""{"items":[],"counts":{}}""")
        routes["GET /api/v1/routers/r2/clients"] = ok("""{"items":[],"counts":{}}""")
    }

    private fun addVm(router: String? = null) = main.track(AddCustomerViewModel(
        AccountsRepository(api), RouterLiveRepository(api), SitesRepository(api), ApartmentsRepository(api),
        initialRouterId = router, initialMac = null, pollMs = 10, maxPolls = 3,
    ))

    @Test fun addCustomerNeedsAnApartmentAndTakesTheOnlyRouterByItself() = runTest(main.dispatcher) {
        addRoutes("""{"items":[$apt,{"id":"ap2","name":"Hilltop","routers":[{"site_id":"r1","router_id":"r1"},{"site_id":"r2","router_id":"r2"}]}]}""")
        val vm = addVm()
        await(vm.state) { it.apartments is Loadable.Ready }
        vm.onUnit("B4")
        vm.onMac("AA:BB:CC:DD:EE:09")
        assertFalse("no apartment chosen yet", vm.state.value.canSubmit)
        vm.onApartment("ap1")
        await(vm.state) { it.routerId == "r1" && it.packages is Loadable.Ready }
        vm.onPackage("p1")
        assertEquals("one router: chosen automatically", 1, vm.state.value.apartmentRouters.size)
        assertFalse(vm.state.value.needsRouterChoice)
        assertTrue(vm.state.value.canSubmit)
        // Several routers: nothing is chosen until the operator does.
        vm.onApartment("ap2")
        await(vm.state) { it.apartmentId == "ap2" && it.packages is Loadable.Ready }
        assertNull(vm.state.value.routerId)
        assertTrue(vm.state.value.needsRouterChoice)
        vm.onPackage("p1")
        assertFalse("router still to pick", vm.state.value.canSubmit)
        vm.setRouter("r2")
        vm.onPackage("p1")
        assertTrue(vm.state.value.canSubmit)
    }

    @Test fun addCustomerMacOfAnotherApartmentIsRefusedInWords() = runTest(main.dispatcher) {
        addRoutes("""{"items":[$apt]}""")
        routes["POST /api/v1/accounts"] = err(
            409, "mac_other_building",
            """{"mac":"AA:BB:CC:DD:EE:09","belongs_to":{"account_id":"a9","code":"FTN-9","customer":"Wanjiru","apartment":{"id":"ap7","name":"Hilltop"},"unit":"D2","site_id":"r2","site_name":"Lavington"}}""",
        )
        val vm = addVm(router = "r1")
        await(vm.state) { it.apartmentId == "ap1" && it.packages is Loadable.Ready }
        vm.onUnit("B4")
        vm.onPackage("p1")
        vm.onMac("AA:BB:CC:DD:EE:09")
        vm.submit()
        await(vm.state) { !it.submitting && it.macOther != null }
        assertEquals("Belongs to Wanjiru (Hilltop, house number D2)", vm.state.value.macOther!!.text)
        assertNull("shown as its own notice, not a generic error", vm.state.value.error)
        assertNull(vm.state.value.added)
        val body = requests("POST /api/v1/accounts").single().body.readUtf8()
        assertTrue(body, body.contains("\"apartment_id\":\"ap1\"") && body.contains("\"unit\":\"B4\""))
    }

    @Test fun addCustomerAsksToConfirmADeviceThatNeverAskedForAnAddressThenResendsWithIt() = runTest(main.dispatcher) {
        addRoutes("""{"items":[$apt]}""")
        routes["POST /api/v1/accounts"] = err(409, "confirm_required", msg = "This device has never asked for an address.")
        val vm = addVm(router = "r1")
        await(vm.state) { it.apartmentId == "ap1" && it.packages is Loadable.Ready }
        vm.onUnit("B4")
        vm.onPackage("p1")
        vm.onMac("AA:BB:CC:DD:EE:09")
        vm.submit()
        await(vm.state) { !it.submitting && it.confirmNoAddress }
        assertNull("the prompt is not a generic error", vm.state.value.error)
        assertNull(vm.state.value.added)
        routes["POST /api/v1/accounts"] = ok("""{"id":"a7","flat":"B4","unit":"B4","code":"FTN-7"}""")
        vm.confirmCreateWithoutAddress()
        await(vm.state) { it.added != null }
        assertFalse(vm.state.value.confirmNoAddress)
        val posts = requests("POST /api/v1/accounts")
        assertEquals(2, posts.size)
        assertFalse(posts[0].body.readUtf8().contains("confirm_no_address"))
        assertTrue(posts[1].body.readUtf8().contains("\"confirm_no_address\":true"))
    }

    // ---- New devices ----

    private val otherDevice = """{"site_id":"r2","site_name":"Lavington","router_id":"r2","router_name":"Lavington","mac":"AA:BB:CC:DD:EE:05","online":true,
        "belongs_to":{"account_id":"a1","code":"FTN-1","customer":"Wanjiru","apartment":{"id":"ap7","name":"Hilltop"},"unit":"A-1","site_id":"r1","site_name":"Kilimani",
        "same_apartment":false,"move_to_choices":[{"id":"ap2","name":"Lavington Court"}]}}"""

    private fun devicesVm() = main.track(NewDevicesViewModel(null, NewDevicesRepository(api), SitesRepository(api), AccountsRepository(api), ApartmentsRepository(api)))

    @Test fun newDeviceOfAnotherApartmentShowsBelongsToAndMovesTheCustomerAfterConfirm() = runTest(main.dispatcher) {
        routes["GET /api/v1/unregistered"] = ok("""{"items":[$otherDevice]}""")
        routes["GET /api/v1/packages"] = ok("""{"items":[]}""")
        routes["POST /api/v1/unregistered/move"] = ok("""{"id":"a1","flat":"A-1","unit":"A-1","apartment":{"id":"ap2","name":"Lavington Court"},"router_id":"r2"}""")
        val vm = devicesVm()
        await(vm.state) { it.devices is Loadable.Ready }
        val d = (vm.state.value.devices as Loadable.Ready<List<NewDevice>>).value.single()
        assertEquals("Belongs to Wanjiru (Hilltop, house number A-1)", d.belongsTo!!.text)
        vm.open(d)
        assertEquals("the only apartment the router serves is preselected", "ap2", vm.state.value.sheet!!.moveApartmentId)
        vm.askMove()
        assertTrue("a confirm comes before the move", vm.state.value.sheet!!.confirmMove)
        assertTrue(requests("POST /api/v1/unregistered/move").isEmpty())
        vm.moveHere()
        await(vm.state) { it.sheet == null && it.message != null }
        val body = requests("POST /api/v1/unregistered/move").single().body.readUtf8()
        assertTrue(body, body.contains("\"mac\":\"AA:BB:CC:DD:EE:05\"") && body.contains("\"site_id\":\"r2\"") && body.contains("\"apartment_id\":\"ap2\""))
    }

    @Test fun createOnANewDeviceNeedsAnApartmentAndOtherApartmentMacIsRefused() = runTest(main.dispatcher) {
        routes["GET /api/v1/unregistered"] = ok("""{"items":[{"site_id":"r1","router_id":"r1","router_name":"Kilimani","mac":"AA:BB:CC:DD:EE:06","online":true}]}""")
        routes["GET /api/v1/packages"] = ok("""{"items":[]}""")
        routes["GET /api/v1/apartments"] = ok("""{"items":[$apt]}""")
        routes["POST /api/v1/unregistered/create"] = err(
            409, "mac_other_building",
            """{"belongs_to":{"customer":"Wanjiru","apartment":{"id":"ap7","name":"Hilltop"},"unit":"A-1"}}""",
        )
        val vm = devicesVm()
        await(vm.state) { it.devices is Loadable.Ready }
        vm.open((vm.state.value.devices as Loadable.Ready<List<NewDevice>>).value.single())
        await(vm.state) { it.sheet?.apartmentId == "ap1" }
        assertEquals("the router's only apartment is preselected", "ap1", vm.state.value.sheet!!.apartmentId)
        vm.setFlat("B4")
        vm.create()
        await(vm.state) { it.sheet?.macOther != null }
        assertNotNull(vm.state.value.sheet!!.macOther)
        assertNull(vm.state.value.sheet!!.error)
        val body = requests("POST /api/v1/unregistered/create").single().body.readUtf8()
        assertTrue(body, body.contains("\"apartment_id\":\"ap1\"") && body.contains("\"unit\":\"B4\""))
    }

    // ---- GPS location ----

    @Test fun apartmentJsonReadsGpsAndTreatsMissingOrNullAsNoPoint() {
        val pinned = ApiJson.decodeFromString(
            Apartment.serializer(),
            """{"id":"ap1","name":"MG One","lat":-1.2921,"lng":36.8219,"map_url":"https://www.google.com/maps/search/?api=1&query=-1.2921%2C36.8219"}""",
        )
        assertEquals(GpsPoint(-1.2921, 36.8219), pinned.gps)
        assertTrue(pinned.mapUrl!!.startsWith("https://www.google.com/maps/search/"))
        val nulls = ApiJson.decodeFromString(Apartment.serializer(), """{"id":"ap2","name":"B","lat":null,"lng":null,"map_url":null}""")
        assertNull(nulls.gps)
        assertNull("an older server sends no GPS fields", ApiJson.decodeFromString(Apartment.serializer(), apt).gps)
        val zero = ApiJson.decodeFromString(Apartment.serializer(), """{"id":"ap3","name":"Z","lat":0,"lng":0}""")
        assertEquals("0,0 is a point, not unset", GpsPoint(0.0, 0.0), zero.gps)
    }

    @Test fun formShowsStoredGpsAndSavesAPastedMapsLink() = runTest(main.dispatcher) {
        routes["GET /api/v1/routers"] = ok("""{"items":[{"id":"r1","name":"Kilimani"}]}""")
        routes["GET /api/v1/apartments/ap1"] = ok(apt.replaceFirst("\"accounts_total\"", "\"lat\":-1.2921,\"lng\":36.8219,\"accounts_total\""))
        routes["PATCH /api/v1/apartments/ap1"] = ok(apt)
        val vm = main.track(ApartmentFormViewModel("ap1", ApartmentsRepository(api), SitesRepository(api)))
        await(vm.state) { !it.loading }
        assertEquals("-1.2921,36.8219", vm.state.value.gpsText)
        // Garbage is refused on the phone, with no call to the server.
        vm.onGps("near the mall")
        vm.save()
        assertEquals(GpsPoint.BAD, vm.state.value.fieldErrors["gps"])
        assertTrue(requests("PATCH /api/v1/apartments/ap1").isEmpty())
        // A Google Maps link is read and sent as numbers.
        vm.onGps("https://www.google.com/maps/place/Kilimani/@-1.3000,36.8000,17z/data=!3m1")
        vm.save()
        await(vm.state) { it.savedId != null }
        val body = requests("PATCH /api/v1/apartments/ap1").single().body.readUtf8()
        assertTrue(body, body.contains("\"lat\":-1.3") && body.contains("\"lng\":36.8") && !body.contains("\"lat\":\""))
    }

    @Test fun clearingTheGpsBoxSendsNullForBoth() = runTest(main.dispatcher) {
        routes["GET /api/v1/routers"] = ok("""{"items":[]}""")
        routes["GET /api/v1/apartments/ap1"] = ok(apt.replaceFirst("\"accounts_total\"", "\"lat\":-1.2921,\"lng\":36.8219,\"accounts_total\""))
        routes["PATCH /api/v1/apartments/ap1"] = ok(apt)
        val vm = main.track(ApartmentFormViewModel("ap1", ApartmentsRepository(api), SitesRepository(api)))
        await(vm.state) { !it.loading }
        vm.onGps("")
        vm.save()
        await(vm.state) { it.savedId != null }
        val body = requests("PATCH /api/v1/apartments/ap1").single().body.readUtf8()
        assertTrue(body, body.contains("\"lat\":null") && body.contains("\"lng\":null"))
    }

    @Test fun serverRefusalOfAPointShowsOnTheGpsField() = runTest(main.dispatcher) {
        routes["GET /api/v1/routers"] = ok("""{"items":[]}""")
        routes["POST /api/v1/apartments"] = err(400, "validation_failed", """{"fields":{"lat":"latitude must be between -90 and 90"}}""")
        val vm = main.track(ApartmentFormViewModel(null, ApartmentsRepository(api), SitesRepository(api)))
        await(vm.state) { true }
        vm.onName("Far")
        vm.onGps("10.0, 20.0")
        vm.save()
        await(vm.state) { !it.saving && it.fieldErrors.isNotEmpty() }
        assertEquals("latitude must be between -90 and 90", vm.state.value.fieldErrors["lat"])
    }
}
