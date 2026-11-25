package ke.co.ftw.ftnadmin.ui

import ke.co.ftw.ftnadmin.core.network.ApiClient
import ke.co.ftw.ftnadmin.core.network.MemoryTokenStore
import ke.co.ftw.ftnadmin.core.network.Session
import ke.co.ftw.ftnadmin.core.network.SessionManager
import ke.co.ftw.ftnadmin.core.network.UserInfo
import ke.co.ftw.ftnadmin.data.accounts.AccountsRepository
import ke.co.ftw.ftnadmin.data.onu.OnuRepository
import ke.co.ftw.ftnadmin.domain.WifiRules
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.onu.OnusViewModel
import ke.co.ftw.ftnadmin.ui.onu.RegisterViewModel
import ke.co.ftw.ftnadmin.ui.onu.WifiViewModel
import ke.co.ftw.ftnadmin.ui.onu.wifiFieldErrors
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

/** The ONU screens' view models, against a fake server. Every helper this file calls is defined in it. */
@OptIn(ExperimentalCoroutinesApi::class)
class OnuViewModelsTest {
    @get:Rule val main = MainDispatcherRule()
    private lateinit var server: MockWebServer
    private lateinit var api: ApiClient
    private lateinit var repo: OnuRepository
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
        repo = OnuRepository(api)
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

    private val onuJson = """{"id":"o1","serial":"ZTEGC1","manufacturer":"ZTE","model":"F670L","data_model":"TR-098","site_id":"s1","online":true,
        "account_id":"a1","account_code":"FTN-1","unit":"A1","link":"inventory","in_inventory":true,
        "wifi":[{"band":"2.4","ssid":"Old2G","enabled":true},{"band":"5","ssid":"Old5G","enabled":true}],
        "desired":{"set":true,"same":true,"ssid":"Old2G","has_password":true,"changed_on_device":true},
        "warnings":["The WAN MAC this ONU reports is not the one in the register."],"use_macs":["AA:BB:CC:DD:EE:99"],
        "last_task":{"id":"t1","kind":"wifi","status":"failed","status_text":"Failed: 9005","fault":"9005"}}"""
    private val accountOnu = """{"onu":$onuJson,"task":{"id":"t1","kind":"wifi","status":"queued","status_text":"Queued"}}"""

    // ---- rules ----

    @Test fun wifiRulesMatchTheServer() {
        assertNull(WifiRules.ssidError("Home Net"))
        assertNotNull(WifiRules.ssidError(""))
        assertNotNull(WifiRules.ssidError("x".repeat(33)))
        assertNotNull(WifiRules.ssidError(" lead"))
        assertNotNull(WifiRules.ssidError("a\u0007b"))
        // 33 bytes with only 17 characters
        assertNotNull(WifiRules.ssidError("é".repeat(17)))
        assertNull(WifiRules.passwordError("12345678"))
        assertNotNull(WifiRules.passwordError("1234567"))
        assertNotNull(WifiRules.passwordError("x".repeat(64)))
        assertNotNull(WifiRules.passwordError("passéword1"))
    }

    @Test fun serverFieldNamesMapToTheForm() {
        val m = wifiFieldErrors(mapOf("ssid" to "a", "password_5g" to "b", "ssid_5g" to "c"))
        assertEquals("a", m["ssid"])
        assertEquals("b", m["password5"])
        assertEquals("c", m["ssid5"])
    }

    // ---- the Wi-Fi card ----

    @Test fun cardLoadsTheOnuWithItsWarningsAndTask() = runTest(main.dispatcher) {
        routes["GET /api/v1/accounts/a1/onu"] = ok(accountOnu)
        val vm = main.track(WifiViewModel("a1", repo))
        await(vm.state) { it.onu is Loadable.Ready }
        val v = (vm.state.value.onu as Loadable.Ready).value
        assertEquals("ZTEGC1", v.onu!!.serial)
        assertEquals(2, v.onu!!.wifi.size)
        assertTrue(v.onu!!.desired.changedOnDevice)
        assertEquals(1, v.onu!!.warnings.size)
        assertTrue(v.task!!.isQueued)
    }

    @Test fun cardWithoutAnOnuIsReadyAndEmpty() = runTest(main.dispatcher) {
        routes["GET /api/v1/accounts/a1/onu"] = ok("""{"onu":null,"task":null}""")
        val vm = main.track(WifiViewModel("a1", repo))
        await(vm.state) { it.onu is Loadable.Ready }
        assertNull((vm.state.value.onu as Loadable.Ready).value.onu)
    }

    @Test fun badWifiInputIsRefusedLocallyWithoutACall() = runTest(main.dispatcher) {
        routes["GET /api/v1/accounts/a1/onu"] = ok(accountOnu)
        val vm = main.track(WifiViewModel("a1", repo))
        await(vm.state) { it.onu is Loadable.Ready }
        vm.openSheet()
        assertEquals("sheet starts with the names the ONU reports", "Old2G", vm.state.value.form.ssid)
        vm.onPassword("short")
        vm.submit()
        advanceUntilIdle()
        assertNotNull(vm.state.value.form.errors["password"])
        assertTrue(vm.state.value.sheet)
        assertTrue(requests("PUT /api/v1/accounts/a1/wifi").isEmpty())
    }

    @Test fun differentBandsAreValidatedSeparately() = runTest(main.dispatcher) {
        routes["GET /api/v1/accounts/a1/onu"] = ok(accountOnu)
        val vm = main.track(WifiViewModel("a1", repo))
        await(vm.state) { it.onu is Loadable.Ready }
        vm.openSheet()
        vm.onSsid("Home")
        vm.onPassword("goodpassword")
        vm.onSame(false)
        vm.onSsid5("")
        vm.submit()
        advanceUntilIdle()
        val errors = vm.state.value.form.errors
        assertNotNull(errors["ssid5"])
        assertNotNull(errors["password5"])
        assertNull(errors["ssid"])
    }

    @Test fun goodWifiChangeIsSentAndTheSheetForgetsThePassword() = runTest(main.dispatcher) {
        routes["GET /api/v1/accounts/a1/onu"] = ok(accountOnu)
        routes["PUT /api/v1/accounts/a1/wifi"] = MockResponse().setResponseCode(202).setBody("""{"task":{"id":"t2","kind":"wifi","status":"queued","status_text":"Queued"}}""")
        val vm = main.track(WifiViewModel("a1", repo))
        await(vm.state) { it.onu is Loadable.Ready }
        vm.openSheet()
        vm.onSame(true)
        vm.onSsid("Home Net")
        vm.onPassword("correct-horse-1")
        vm.submit()
        await(vm.state) { !it.busy && !it.sheet }
        val body = requests("PUT /api/v1/accounts/a1/wifi").single().body.readUtf8()
        assertTrue(body, body.contains("\"ssid\":\"Home Net\""))
        assertTrue(body, body.contains("\"password\":\"correct-horse-1\""))
        assertTrue(body, body.contains("\"same_for_both\":true"))
        assertFalse("5 GHz fields are not sent when the same name is used", body.contains("ssid_5g"))
        assertEquals("", vm.state.value.form.password)
        assertNotNull(vm.state.value.info)
    }

    @Test fun serverValidationReasonShowsOnTheField() = runTest(main.dispatcher) {
        routes["GET /api/v1/accounts/a1/onu"] = ok(accountOnu)
        routes["PUT /api/v1/accounts/a1/wifi"] = err(400, "validation_failed", """{"fields":{"password":"the Wi-Fi password may use printable English letters only"}}""")
        val vm = main.track(WifiViewModel("a1", repo))
        await(vm.state) { it.onu is Loadable.Ready }
        vm.openSheet()
        vm.onSame(true)
        vm.onSsid("Home")
        vm.onPassword("goodpassword")
        vm.submit()
        await(vm.state) { !it.busy && it.form.errors.isNotEmpty() }
        assertTrue(vm.state.value.sheet)
        assertNotNull(vm.state.value.form.errors["password"])
    }

    @Test fun rebootAsksFirstThenQueues() = runTest(main.dispatcher) {
        routes["GET /api/v1/accounts/a1/onu"] = ok(accountOnu)
        routes["POST /api/v1/accounts/a1/onu/reboot"] = MockResponse().setResponseCode(202).setBody("""{"task":{"id":"t3","kind":"reboot","status":"queued","status_text":"Queued"}}""")
        val vm = main.track(WifiViewModel("a1", repo))
        await(vm.state) { it.onu is Loadable.Ready }
        vm.askReboot()
        assertTrue(vm.state.value.confirmReboot)
        advanceUntilIdle()
        assertTrue(requests("POST /api/v1/accounts/a1/onu/reboot").isEmpty())
        vm.cancelReboot()
        assertFalse(vm.state.value.confirmReboot)
        vm.askReboot()
        vm.reboot()
        await(vm.state) { !it.busy && it.info != null }
        assertEquals(1, requests("POST /api/v1/accounts/a1/onu/reboot").size)
        assertFalse(vm.state.value.confirmReboot)
    }

    // ---- the router's ONUs ----

    @Test fun routerOnusLoadAndLink() = runTest(main.dispatcher) {
        routes["GET /api/v1/sites/s1/onus"] = ok("""{"items":[{"id":"o2","serial":"SN2","manufacturer":"ZTE","model":"F670L"},$onuJson],"management_network":"10.6.3.0/24"}""")
        routes["POST /api/v1/onus/o2/link"] = ok("""{"id":"o2","serial":"SN2","account_id":"a1"}""")
        val vm = main.track(OnusViewModel("s1", repo, AccountsRepository(api)))
        await(vm.state) { it.items is Loadable.Ready }
        val list = (vm.state.value.items as Loadable.Ready).value
        assertEquals(2, list.size)
        assertFalse(list[0].isLinked)
        assertTrue(list[1].isLinked)
        vm.startLink(list[0])
        assertEquals("o2", vm.state.value.linking!!.id)
        vm.link(ke.co.ftw.ftnadmin.domain.Account(id = "a1", flat = "A1"))
        await(vm.state) { !it.busy && it.linking == null }
        val body = requests("POST /api/v1/onus/o2/link").single().body.readUtf8()
        assertTrue(body, body.contains("\"account_id\":\"a1\""))
    }

    @Test fun linkRefusalStaysOpenWithTheReason() = runTest(main.dispatcher) {
        routes["GET /api/v1/sites/s1/onus"] = ok("""{"items":[{"id":"o2","serial":"SN2"}]}""")
        routes["POST /api/v1/onus/o2/link"] = err(409, "bad_state", msg = "that house already has an ONU linked: unlink it first")
        val vm = main.track(OnusViewModel("s1", repo, AccountsRepository(api)))
        await(vm.state) { it.items is Loadable.Ready }
        vm.startLink((vm.state.value.items as Loadable.Ready).value[0])
        vm.link(ke.co.ftw.ftnadmin.domain.Account(id = "a1", flat = "A1"))
        await(vm.state) { !it.busy && it.error != null }
        assertTrue(vm.state.value.error!!.contains("already has an ONU"))
        assertNotNull(vm.state.value.linking)
    }

    @Test fun reportedMacAndRegisterAddGoToTheServer() = runTest(main.dispatcher) {
        routes["GET /api/v1/sites/s1/onus"] = ok("""{"items":[$onuJson]}""")
        routes["POST /api/v1/accounts/a1/onu/use-reported-mac"] = ok("""{}""")
        val vm = main.track(OnusViewModel("s1", repo, AccountsRepository(api)))
        await(vm.state) { it.items is Loadable.Ready }
        val onu = (vm.state.value.items as Loadable.Ready).value[0]
        vm.useReportedMac(onu, onu.useMacs.first())
        await(vm.state) { !it.busy && it.info != null }
        val body = requests("POST /api/v1/accounts/a1/onu/use-reported-mac").single().body.readUtf8()
        assertTrue(body, body.contains("AA:BB:CC:DD:EE:99"))
    }

    // ---- task states, confirmation, quarantine ----

    private val acceptedOnu = """{"onu":$onuJson,"task":{"id":"t9","kind":"wifi","status":"accepted","status_text":"Accepted: applies after the ONU restarts","after_reboot":true,"can_confirm":true}}"""

    @Test fun taskStatesAreReadFromTheServer() = runTest(main.dispatcher) {
        routes["GET /api/v1/accounts/a1/onu"] = ok(acceptedOnu)
        val vm = main.track(WifiViewModel("a1", repo))
        await(vm.state) { it.onu is Loadable.Ready }
        val t = (vm.state.value.onu as Loadable.Ready).value.task!!
        assertTrue(t.isAccepted)
        assertTrue(t.afterReboot)
        assertTrue(t.canConfirm)
        assertFalse(t.isQueued)
        val expired = kotlinx.serialization.json.Json { ignoreUnknownKeys = true }
            .decodeFromString(ke.co.ftw.ftnadmin.data.onu.OnuTask.serializer(), """{"id":"t8","kind":"reboot","status":"expired","status_text":"Expired","can_cancel":false}""")
        assertTrue(expired.isExpired)
        assertFalse(expired.isFailed)
    }

    @Test fun itWorksConfirmsTheAcceptedChange() = runTest(main.dispatcher) {
        routes["GET /api/v1/accounts/a1/onu"] = ok(acceptedOnu)
        routes["POST /api/v1/accounts/a1/onu/confirm"] = ok("""{"task":{"id":"t9","kind":"wifi","status":"confirmed","status_text":"Confirmed working"}}""")
        val vm = main.track(WifiViewModel("a1", repo))
        await(vm.state) { it.onu is Loadable.Ready }
        vm.confirm()
        await(vm.state) { !it.busy && it.info != null }
        assertEquals(1, requests("POST /api/v1/accounts/a1/onu/confirm").size)
    }

    @Test fun itDidNotWorkAsksForAnotherTry() = runTest(main.dispatcher) {
        routes["GET /api/v1/accounts/a1/onu"] = ok(acceptedOnu)
        routes["POST /api/v1/accounts/a1/onu/reject"] = MockResponse().setResponseCode(202).setBody("""{"task":{"id":"t10","kind":"wifi","status":"queued","status_text":"Queued"}}""")
        val vm = main.track(WifiViewModel("a1", repo))
        await(vm.state) { it.onu is Loadable.Ready }
        vm.reject()
        await(vm.state) { !it.busy && it.info != null }
        assertEquals(1, requests("POST /api/v1/accounts/a1/onu/reject").size)
    }

    @Test fun cancelSendsTheTaskId() = runTest(main.dispatcher) {
        routes["GET /api/v1/accounts/a1/onu"] = ok(accountOnu)
        routes["POST /api/v1/onu-tasks/t1/cancel"] = ok("""{"ok":true}""")
        val vm = main.track(WifiViewModel("a1", repo))
        await(vm.state) { it.onu is Loadable.Ready }
        vm.cancelTask("t1")
        await(vm.state) { !it.busy && it.info != null }
        assertEquals(1, requests("POST /api/v1/onu-tasks/t1/cancel").size)
    }

    @Test fun confirmRefusalShowsTheReason() = runTest(main.dispatcher) {
        routes["GET /api/v1/accounts/a1/onu"] = ok(accountOnu)
        routes["POST /api/v1/accounts/a1/onu/confirm"] = err(409, "bad_state", msg = "there is no Wi-Fi change waiting for a confirmation on this house's ONU")
        val vm = main.track(WifiViewModel("a1", repo))
        await(vm.state) { it.onu is Loadable.Ready }
        vm.confirm()
        await(vm.state) { !it.busy && it.error != null }
        assertTrue(vm.state.value.error!!.contains("no Wi-Fi change waiting"))
    }

    private val quarantinedJson = """{"id":"o5","serial":"SNQ5","manufacturer":"V-SOL","site_id":"s1","shared_login_only":true,"returned":false,
        "quarantine":{"reason":"seen from another router","count":2,"ip":"10.6.4.50","mac":"AA:BB:CC:00:00:50","site_name":"Block B"}}"""

    @Test fun quarantinedOnuIsShownAndApproved() = runTest(main.dispatcher) {
        routes["GET /api/v1/sites/s1/onus"] = ok("""{"items":[$quarantinedJson]}""")
        routes["POST /api/v1/onus/o5/approve"] = ok("""{"id":"o5","serial":"SNQ5"}""")
        val vm = main.track(OnusViewModel("s1", repo, AccountsRepository(api)))
        await(vm.state) { it.items is Loadable.Ready }
        val onu = (vm.state.value.items as Loadable.Ready).value.single()
        assertTrue(onu.isQuarantined)
        assertTrue(onu.sharedLoginOnly)
        assertEquals("seen from another router", onu.quarantine!!.reason)
        vm.approve(onu)
        await(vm.state) { !it.busy && it.info != null }
        assertEquals(1, requests("POST /api/v1/onus/o5/approve").size)
    }

    @Test fun quarantinedOnuCanBeBlockedAndUnblocked() = runTest(main.dispatcher) {
        routes["GET /api/v1/sites/s1/onus"] = ok("""{"items":[$quarantinedJson]}""")
        routes["POST /api/v1/onus/o5/block"] = ok("""{"id":"o5","serial":"SNQ5","blocked":true}""")
        routes["POST /api/v1/onus/o5/unblock"] = ok("""{"id":"o5","serial":"SNQ5"}""")
        val vm = main.track(OnusViewModel("s1", repo, AccountsRepository(api)))
        await(vm.state) { it.items is Loadable.Ready }
        val onu = (vm.state.value.items as Loadable.Ready).value.single()
        vm.block(onu)
        await(vm.state) { !it.busy && it.info != null }
        vm.clearInfo()
        vm.unblock(onu)
        await(vm.state) { !it.busy && it.info != null }
        assertEquals(1, requests("POST /api/v1/onus/o5/block").size)
        assertEquals(1, requests("POST /api/v1/onus/o5/unblock").size)
    }

    private val waitingJson = """{"id":"o7","serial":"SNW7","manufacturer":"V-SOL","site_id":"s1",
        "mac_wait":{"text":"Waiting: management MAC doesn't match the registered ONU yet","lease_mac":"AA:BB:CC:00:00:90","registered_mac":"AA:BB:CC:00:00:10","can_trust":true}}"""

    @Test fun onuWaitingForItsMacIsShownAndTrusted() = runTest(main.dispatcher) {
        routes["GET /api/v1/sites/s1/onus"] = ok("""{"items":[$waitingJson]}""")
        routes["POST /api/v1/onus/o7/trust"] = ok("""{"id":"o7","serial":"SNW7"}""")
        val vm = main.track(OnusViewModel("s1", repo, AccountsRepository(api)))
        await(vm.state) { it.items is Loadable.Ready }
        val onu = (vm.state.value.items as Loadable.Ready).value.single()
        assertTrue(onu.isWaitingForMac)
        assertEquals("AA:BB:CC:00:00:90", onu.macWait!!.leaseMac)
        assertEquals("AA:BB:CC:00:00:10", onu.macWait!!.registeredMac)
        assertTrue(onu.macWait!!.canTrust)
        vm.trust(onu)
        await(vm.state) { !it.busy && it.info != null }
        val body = requests("POST /api/v1/onus/o7/trust").single().body.readUtf8()
        assertTrue(body, body.contains("\"mac\":\"AA:BB:CC:00:00:90\""))
    }

    @Test fun approveWithNothingToApproveShowsTheReason() = runTest(main.dispatcher) {
        routes["GET /api/v1/sites/s1/onus"] = ok("""{"items":[{"id":"o6","serial":"SN6"}]}""")
        routes["POST /api/v1/onus/o6/approve"] = err(409, "bad_state", msg = "that ONU is not waiting for an approval")
        val vm = main.track(OnusViewModel("s1", repo, AccountsRepository(api)))
        await(vm.state) { it.items is Loadable.Ready }
        vm.approve((vm.state.value.items as Loadable.Ready).value.single())
        await(vm.state) { !it.busy && it.error != null }
        assertTrue(vm.state.value.error!!.contains("not waiting for an approval"))
    }

    // ---- the register ----

    @Test fun registerListsOnusAndRejectsABadMacLocally() = runTest(main.dispatcher) {
        routes["GET /api/v1/equipment"] = ok("""{"items":[{"id":"e1","kind":"ont","serial":"ZTEGC1","mac":"AA:BB:CC:DD:EE:01","status":"in_stock"}]}""")
        val vm = main.track(RegisterViewModel(repo))
        await(vm.state) { it.items is Loadable.Ready }
        assertEquals(1, (vm.state.value.items as Loadable.Ready).value.size)
        vm.openAdd()
        vm.onSerial("ZTEGC9")
        vm.onMac("AA:BB")
        vm.save()
        advanceUntilIdle()
        assertNotNull(vm.state.value.form.errors["mac"])
        assertTrue(requests("POST /api/v1/equipment").isEmpty())
        vm.onMac("")
        vm.save()
        advanceUntilIdle()
        assertNotNull("an empty MAC is an error too", vm.state.value.form.errors["mac"])
    }

    @Test fun registerAddSendsTheNormalisedPair() = runTest(main.dispatcher) {
        routes["GET /api/v1/equipment"] = ok("""{"items":[]}""")
        routes["POST /api/v1/equipment"] = MockResponse().setResponseCode(201).setBody("""{"id":"e2","kind":"ont","serial":"ZTEGC9","mac":"AA:BB:CC:DD:EE:09","status":"in_stock"}""")
        val vm = main.track(RegisterViewModel(repo))
        await(vm.state) { it.items is Loadable.Ready }
        vm.openAdd()
        vm.onSerial("ZTEGC9")
        vm.onGpon("ZTEG-9")
        vm.onMac("aa-bb-cc-dd-ee-09")
        vm.onModel("F670L")
        vm.save()
        await(vm.state) { !it.busy && !it.adding }
        val body = requests("POST /api/v1/equipment").single().body.readUtf8()
        assertTrue(body, body.contains("\"mac\":\"AA:BB:CC:DD:EE:09\""))
        assertTrue(body, body.contains("\"gpon_serial\":\"ZTEG-9\""))
        assertTrue(body, body.contains("\"serial\":\"ZTEGC9\""))
    }

    @Test fun registerDuplicateSurfacesTheServerReason() = runTest(main.dispatcher) {
        routes["GET /api/v1/equipment"] = ok("""{"items":[]}""")
        routes["POST /api/v1/equipment"] = err(409, "bad_state", msg = "an ONT with serial number ZTEGC9 is already in the register")
        val vm = main.track(RegisterViewModel(repo))
        await(vm.state) { it.items is Loadable.Ready }
        vm.openAdd()
        vm.onSerial("ZTEGC9")
        vm.onMac("AA:BB:CC:DD:EE:09")
        vm.save()
        await(vm.state) { !it.busy && it.error != null }
        assertTrue(vm.state.value.error!!.contains("already in the register"))
        assertTrue(vm.state.value.adding)
    }

    // ---- a 2.4 GHz-only box, and the current Wi-Fi password ----

    private val singleRadioOnu = """{"onu":{"id":"o1","serial":"VSOL1","manufacturer":"V-SOL","model":"VSOL V414","account_id":"a1","bands":["2.4"],"has_5ghz":false,"wifi":[{"band":"2.4","ssid":"Old2G","enabled":true}]},"task":null}"""

    @Test fun singleRadioBoxHasOneNetworkAndSendsSameForBoth() = runTest(main.dispatcher) {
        routes["GET /api/v1/accounts/a1/onu"] = ok(singleRadioOnu)
        routes["PUT /api/v1/accounts/a1/wifi"] = MockResponse().setResponseCode(202).setBody("""{"task":{"id":"t2","kind":"wifi","status":"queued","status_text":"Queued"}}""")
        val vm = main.track(WifiViewModel("a1", repo))
        await(vm.state) { it.onu is Loadable.Ready }
        val onu = (vm.state.value.onu as Loadable.Ready).value.onu!!
        assertFalse(onu.has5ghz)
        assertEquals(listOf("2.4"), onu.bands)
        vm.openSheet()
        assertTrue("one name and password for the box", vm.state.value.form.same)
        // even if the same-for-both switch was somehow off, the 5 GHz fields are not required or sent
        vm.onSsid("Home")
        vm.onPassword("goodpassword")
        vm.onSame(false)
        vm.submit()
        await(vm.state) { !it.busy && !it.sheet }
        val body = requests("PUT /api/v1/accounts/a1/wifi").single().body.readUtf8()
        assertTrue(body, body.contains("\"same_for_both\":true"))
        assertFalse(body, body.contains("ssid_5g"))
    }

    @Test fun oldServerWithoutBandInfoKeepsBothBands() = runTest(main.dispatcher) {
        routes["GET /api/v1/accounts/a1/onu"] = ok(accountOnu)
        val vm = main.track(WifiViewModel("a1", repo))
        await(vm.state) { it.onu is Loadable.Ready }
        assertTrue((vm.state.value.onu as Loadable.Ready).value.onu!!.has5ghz)
    }

    @Test fun showingTheWifiPasswordCallsTheServerAndForgetsOnClose() = runTest(main.dispatcher) {
        routes["GET /api/v1/accounts/a1/onu"] = ok(singleRadioOnu)
        routes["POST /api/v1/accounts/a1/wifi/password"] =
            ok("""{"networks":[{"band":"2.4","ssid":"Home","password":"house24pass","source":"saved"}],"asked_onu":true,"note":"The ONU did not return its password."}""")
        val vm = main.track(WifiViewModel("a1", repo))
        await(vm.state) { it.onu is Loadable.Ready }
        vm.showPassword()
        await(vm.state) { !it.busy && it.reveal != null }
        val r = vm.state.value.reveal!!
        assertEquals("house24pass", r.networks.single().password)
        assertEquals("saved", r.networks.single().source)
        assertEquals(1, requests("POST /api/v1/accounts/a1/wifi/password").size)
        vm.closeReveal()
        assertNull(vm.state.value.reveal)
    }
}
