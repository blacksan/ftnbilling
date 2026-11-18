package ke.co.ftw.ftnadmin.ui

import ke.co.ftw.ftnadmin.core.network.ApiClient
import ke.co.ftw.ftnadmin.core.network.MemoryTokenStore
import ke.co.ftw.ftnadmin.core.network.Session
import ke.co.ftw.ftnadmin.core.network.SessionManager
import ke.co.ftw.ftnadmin.core.network.UserInfo
import ke.co.ftw.ftnadmin.data.accounts.AccountsRepository
import ke.co.ftw.ftnadmin.data.payments.PaymentsRepository
import ke.co.ftw.ftnadmin.data.sites.NewDevicesRepository
import ke.co.ftw.ftnadmin.data.sites.SitesRepository
import ke.co.ftw.ftnadmin.domain.NewDevice
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.customers.CustomerDetailViewModel
import ke.co.ftw.ftnadmin.ui.customers.DetailDialog
import ke.co.ftw.ftnadmin.ui.customers.macConflictText
import ke.co.ftw.ftnadmin.ui.newdevices.NewDevicesViewModel
import ke.co.ftw.ftnadmin.ui.newdevices.SheetTab
import ke.co.ftw.ftnadmin.ui.payments.PaymentDialog
import ke.co.ftw.ftnadmin.ui.payments.PaymentsViewModel
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

@OptIn(ExperimentalCoroutinesApi::class)
class ViewModelsTest {
    @get:Rule val main = MainDispatcherRule()
    private lateinit var server: MockWebServer
    private lateinit var api: ApiClient
    private val routes = mutableMapOf<String, MockResponse>()
    private val seen = mutableListOf<String>()

    @Before fun setUp() {
        server = MockWebServer()
        server.dispatcher = object : Dispatcher() {
            override fun dispatch(request: RecordedRequest): MockResponse {
                val key = "${request.method} ${request.requestUrl!!.encodedPath}"
                synchronized(seen) { seen += key }
                return routes[key] ?: MockResponse().setResponseCode(404).setBody("""{"error":{"code":"not_found","message":"nope"}}""")
            }
        }
        server.start()
        val sessions = SessionManager(MemoryTokenStore(Session("a", "", "r", "", "d", UserInfo("1", "b", "owner"))))
        sessions.restore()
        api = ApiClient.create(server.url("/").toString(), sessions)
    }

    @After fun tearDown() = server.shutdown()

    private fun ok(body: String) = MockResponse().setBody(body)

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

    private val account = """{"id":"a1","code":"FTN-1","site_id":"s1","site_name":"Kilimani","flat":"A1","mac":"AA:BB:CC:DD:EE:01","state":"active"}"""

    // ---- customer detail ----

    @Test fun macEditShowsOwningFlatAndRouterOn409() = runTest(main.dispatcher) {
        routes["GET /api/v1/accounts/a1"] = ok(account)
        routes["GET /api/v1/packages"] = ok("""{"items":[]}""")
        routes["PUT /api/v1/accounts/a1/mac"] = err(
            409, "mac_in_use",
            """{"mac":"AA:BB:CC:DD:EE:02","account":{"id":"a2","code":"FTN-2","flat":"C7","site_id":"s2","site_name":"Lavington Court"}}""",
        )
        val vm = CustomerDetailViewModel("a1", AccountsRepository(api))
        await(vm.state) { it.account is Loadable.Ready }

        vm.open(DetailDialog.EditMac)
        vm.onMacInput("aa-bb-cc-dd-ee-02")
        assertNull(vm.state.value.macFieldError)
        vm.saveMac()
        await(vm.state) { !it.busy && it.macOwner != null }

        val s = vm.state.value
        assertEquals(DetailDialog.EditMac, s.dialog)
        val text = macConflictText(s.macOwner!!)
        assertTrue(text, text.contains("flat C7"))
        assertTrue(text, text.contains("Lavington Court"))
        assertTrue(text, text.contains("FTN-2"))
        assertNull("conflict is shown as its own notice, not a generic error", s.error)
    }

    @Test fun badMacIsRejectedLocallyWithoutACall() = runTest(main.dispatcher) {
        routes["GET /api/v1/accounts/a1"] = ok(account)
        routes["GET /api/v1/packages"] = ok("""{"items":[]}""")
        val vm = CustomerDetailViewModel("a1", AccountsRepository(api))
        await(vm.state) { it.account is Loadable.Ready }
        vm.open(DetailDialog.EditMac)
        vm.onMacInput("AA:BB:CC")
        assertNotNull(vm.state.value.macFieldError)
        vm.saveMac()
        advanceUntilIdle()
        assertTrue(synchronized(seen) { seen.none { it.startsWith("PUT") } })
    }

    @Test fun successfulMacChangeClosesDialogAndKeepsDetailExtras() = runTest(main.dispatcher) {
        routes["GET /api/v1/accounts/a1"] = ok(account.dropLast(1) + ""","events":[{"time":"2026-01-01T00:00:00Z","msg":"hello"}]}""")
        routes["GET /api/v1/packages"] = ok("""{"items":[]}""")
        routes["PUT /api/v1/accounts/a1/mac"] = ok(account.replace("EE:01", "EE:09"))
        val vm = CustomerDetailViewModel("a1", AccountsRepository(api))
        await(vm.state) { it.account is Loadable.Ready }
        vm.open(DetailDialog.EditMac)
        vm.onMacInput("AA:BB:CC:DD:EE:09")
        vm.saveMac()
        await(vm.state) { it.dialog == DetailDialog.None && !it.busy }
        val a = (vm.state.value.account as Loadable.Ready).value
        assertEquals("AA:BB:CC:DD:EE:09", a.mac)
        assertEquals("events survive an update response without them", 1, a.events.size)
    }

    @Test fun lowerPlanAsksBeforeForcing() = runTest(main.dispatcher) {
        routes["GET /api/v1/accounts/a1"] = ok(account)
        routes["GET /api/v1/packages"] = ok("""{"items":[]}""")
        routes["POST /api/v1/accounts/a1/extend"] = err(409, "lower_plan_active", msg = "A faster plan is running.")
        val vm = CustomerDetailViewModel("a1", AccountsRepository(api))
        await(vm.state) { it.account is Loadable.Ready }
        vm.open(DetailDialog.Extend)
        vm.extendTracked("p1", null)
        await(vm.state) { it.dialog is DetailDialog.LowerPlan }
        assertEquals("A faster plan is running.", (vm.state.value.dialog as DetailDialog.LowerPlan).message)
    }

    // ---- new devices ----

    private fun dev(mac: String, online: Boolean, seen: String) =
        """{"site_id":"s1","site_name":"Kilimani","mac":"$mac","online":$online,"last_seen":"$seen"}"""

    @Test fun newDevicesAreSortedOnlineFirstThenMostRecent() = runTest(main.dispatcher) {
        routes["GET /api/v1/unregistered"] = ok(
            """{"items":[${dev("00:00:00:00:00:01", false, "2026-01-03T00:00:00Z")},${dev("00:00:00:00:00:02", true, "2026-01-01T00:00:00Z")},${dev("00:00:00:00:00:03", true, "2026-01-02T00:00:00Z")}]}""",
        )
        routes["GET /api/v1/packages"] = ok("""{"items":[]}""")
        val vm = NewDevicesViewModel(null, NewDevicesRepository(api), SitesRepository(api), AccountsRepository(api))
        await(vm.state) { it.devices is Loadable.Ready }
        val macs = (vm.state.value.devices as Loadable.Ready<List<NewDevice>>).value.map { it.mac.takeLast(2) }
        assertEquals(listOf("03", "02", "01"), macs)
    }

    @Test fun attachConflictNamesTheOwner() = runTest(main.dispatcher) {
        routes["GET /api/v1/unregistered"] = ok(
            """{"items":[{"site_id":"s1","site_name":"Kilimani","mac":"AA:BB:CC:DD:EE:05","online":true,"suggested_account":{"id":"a1","code":"FTN-1","flat":"A1"}}]}""",
        )
        routes["GET /api/v1/packages"] = ok("""{"items":[]}""")
        routes["POST /api/v1/unregistered/attach"] = err(
            409, "mac_in_use",
            """{"mac":"AA:BB:CC:DD:EE:05","account":{"id":"a9","code":"FTN-9","flat":"D2","site_id":"s3","site_name":"Westlands Plaza"}}""",
        )
        val vm = NewDevicesViewModel(null, NewDevicesRepository(api), SitesRepository(api), AccountsRepository(api))
        await(vm.state) { it.devices is Loadable.Ready }
        val device = (vm.state.value.devices as Loadable.Ready<List<NewDevice>>).value.single()
        vm.open(device)
        assertEquals("suggested account is preselected", "a1", vm.state.value.sheet!!.selected!!.id)
        vm.attach()
        await(vm.state) { it.sheet?.macOwner != null }
        val o = vm.state.value.sheet!!.macOwner!!
        assertEquals("D2", o.flat)
        assertEquals("Westlands Plaza", o.siteName)
        assertNull(vm.state.value.sheet!!.error)
        assertEquals(SheetTab.Attach, vm.state.value.sheet!!.tab)
    }

    @Test fun pullToRefreshAsksEachRouterThenReloads() = runTest(main.dispatcher) {
        routes["GET /api/v1/unregistered"] = ok("""{"items":[]}""")
        routes["GET /api/v1/packages"] = ok("""{"items":[]}""")
        routes["GET /api/v1/routers"] = ok("""{"items":[{"id":"s1","name":"A"},{"id":"s2","name":"B"}]}""")
        routes["POST /api/v1/routers/s1/unregistered/refresh"] = MockResponse().setResponseCode(202)
        routes["POST /api/v1/routers/s2/unregistered/refresh"] = err(503, "db_unavailable")
        val vm = NewDevicesViewModel(null, NewDevicesRepository(api), SitesRepository(api), AccountsRepository(api))
        await(vm.state) { it.devices is Loadable.Ready }
        synchronized(seen) { seen.clear() }
        vm.refresh()
        await(vm.state) { !it.refreshing }
        val calls = synchronized(seen) { seen.toList() }
        assertTrue(calls.toString(), calls.contains("POST /api/v1/routers/s1/unregistered/refresh"))
        assertTrue(calls.toString(), calls.contains("POST /api/v1/routers/s2/unregistered/refresh"))
        assertEquals("reload happens after the refreshes", "GET /api/v1/unregistered", calls.last())
    }

    // ---- payments ----

    @Test fun duplicatePaymentOffersRecordAnyway() = runTest(main.dispatcher) {
        routes["GET /api/v1/payments"] = ok("""{"items":[],"totals":{"count":0,"revenue_kes":0}}""")
        routes["GET /api/v1/packages"] = ok("""{"items":[]}""")
        routes["POST /api/v1/payments"] = err(409, "duplicate_payment", msg = "Code already recorded.")
        val vm = PaymentsViewModel(PaymentsRepository(api), AccountsRepository(api))
        await(vm.state) { !it.loading }
        vm.openRecord()
        vm.editForm { it.copy(account = ke.co.ftw.ftnadmin.domain.Account(id = "a1", flat = "A1"), amount = "1500", transCode = "ABC123") }
        assertTrue(vm.state.value.form.valid)
        vm.record()
        await(vm.state) { it.duplicatePrompt != null }
        assertEquals(PaymentDialog.Record, vm.state.value.dialog)
        assertNull(vm.state.value.dialogError)
    }
}
