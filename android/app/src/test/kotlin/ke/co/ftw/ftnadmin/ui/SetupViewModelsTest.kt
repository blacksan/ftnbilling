package ke.co.ftw.ftnadmin.ui

import ke.co.ftw.ftnadmin.core.network.ApiClient
import ke.co.ftw.ftnadmin.core.network.MemoryTokenStore
import ke.co.ftw.ftnadmin.core.network.Session
import ke.co.ftw.ftnadmin.core.network.SessionManager
import ke.co.ftw.ftnadmin.core.network.UserInfo
import ke.co.ftw.ftnadmin.data.sites.CheckStatus
import ke.co.ftw.ftnadmin.data.sites.SiteSetupRepository
import ke.co.ftw.ftnadmin.data.sites.SitesRepository
import ke.co.ftw.ftnadmin.ui.routers.setup.SetupJobViewModel
import ke.co.ftw.ftnadmin.ui.routers.setup.SetupViewModel
import ke.co.ftw.ftnadmin.ui.routers.setup.canRunFix
import ke.co.ftw.ftnadmin.data.sites.CheckFix
import ke.co.ftw.ftnadmin.ui.common.Loadable
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

@OptIn(ExperimentalCoroutinesApi::class)
class SetupViewModelsTest {
    @get:Rule val main = MainDispatcherRule()
    private lateinit var server: MockWebServer
    private lateinit var api: ApiClient
    private val routes = mutableMapOf<String, MockResponse>()
    private val sequences = mutableMapOf<String, MutableList<MockResponse>>()
    private val log = mutableListOf<RecordedRequest>()
    private val owner = UserInfo("1", "b", "owner")
    private val viewer = UserInfo("2", "v", "viewer")

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
        api = ApiClient.create(server.url("/").toString(), sessions)
        routes["GET /api/v1/routers/s1"] = ok("""{"id":"s1","name":"Kilimani","access_mode":"hotspot"}""")
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

    private fun requests(prefix: String) = synchronized(log) { log.filter { "${it.method} ${it.requestUrl!!.encodedPath}".startsWith(prefix) } }

    private fun setupVm() = SetupViewModel("s1", SiteSetupRepository(api), SitesRepository(api))

    private fun item(id: String, status: String, fix: String = "") =
        """{"id":"$id","group":"network","status":"$status","blocks_cutover":true,"title":"T $id","detail":"d"$fix}"""

    private fun checks(vararg items: String, ready: Boolean = false) =
        """{"site_id":"s1","generated_at":"2026-11-18T10:00:00Z","router":{"reachable":true,"version":"7.25.1"},""" +
            """"summary":{"ok":0,"warn":0,"fail":1,"unknown":0,"cutover_ready":$ready},"join_report":null,"items":[${items.joinToString(",")}]}"""

    // ---- checks ----

    @Test fun checksLoadThenPullToRefreshUsesFresh() = runTest(main.dispatcher) {
        routes["GET /api/v1/routers/s1/checks"] = ok(checks(item("dns.remote_requests", "fail")))
        val vm = setupVm()
        await(vm.state) { it.checks is Loadable.Ready && it.mode == "hotspot" }
        val first = requests("GET /api/v1/routers/s1/checks").single()
        assertNull("first load may use the server cache", first.requestUrl!!.queryParameter("fresh"))

        vm.refresh()
        await(vm.state) { !it.refreshing && requests("GET /api/v1/routers/s1/checks").size == 2 }
        assertEquals("1", requests("GET /api/v1/routers/s1/checks")[1].requestUrl!!.queryParameter("fresh"))
        val c = (vm.state.value.checks as Loadable.Ready).value
        assertEquals(CheckStatus.FAIL, c.items.single().status)
        assertTrue(c.items.single().blocksCutover)
    }

    @Test fun routerUnreachableIsExplained() = runTest(main.dispatcher) {
        routes["GET /api/v1/routers/s1/checks"] = err(503, "router_unreachable", msg = "dial tcp timeout")
        val vm = setupVm()
        await(vm.state) { it.checks is Loadable.Failed }
        val msg = (vm.state.value.checks as Loadable.Failed).message
        assertTrue(msg, msg.contains("cannot reach this router"))
    }

    // ---- fixes ----

    @Test fun autoFixSuccessReplacesItemAndRereadsFresh() = runTest(main.dispatcher) {
        val fixJson = ""","fix":{"kind":"auto","label":"Fix traffic flow","role":"operator"}"""
        routes["GET /api/v1/routers/s1/checks"] = ok(checks(item("traffic_flow", "warn", fixJson)))
        routes["POST /api/v1/routers/s1/checks/traffic_flow/fix"] = ok("""{"item":${item("traffic_flow", "ok")},"applied":["set interface"]}""")
        val vm = setupVm()
        await(vm.state) { it.checks is Loadable.Ready }
        val it0 = (vm.state.value.checks as Loadable.Ready).value.items.single()

        vm.requestFix(it0)
        await(vm.state) { s -> s.fixing.isEmpty() && s.message?.contains("set interface") == true }
        val posted = requests("POST /api/v1/routers/s1/checks/traffic_flow/fix").single()
        assertFalse("auto fixes carry no confirm flag", posted.body.readUtf8().contains("confirm"))
        val c = (vm.state.value.checks as Loadable.Ready).value
        assertEquals(CheckStatus.OK, c.items.single().status)
        await(vm.state) { requests("GET /api/v1/routers/s1/checks").any { r -> r.requestUrl!!.queryParameter("fresh") == "1" } }
    }

    @Test fun confirmFixIsSentDirectlyWithoutASecondConfirmStep() = runTest(main.dispatcher) {
        val fixJson = ""","fix":{"kind":"confirm","label":"Remove OLT port","confirm_text":"Learned MACs: AA","role":"owner"}"""
        routes["GET /api/v1/routers/s1/checks"] = ok(checks(item("olt.bridge_conflict", "fail", fixJson)))
        routes["POST /api/v1/routers/s1/checks/olt.bridge_conflict/fix"] = ok("""{"item":${item("olt.bridge_conflict", "ok")},"applied":[]}""")
        val vm = setupVm()
        await(vm.state) { it.checks is Loadable.Ready }
        val i = (vm.state.value.checks as Loadable.Ready).value.items.single()

        vm.requestFix(i)
        await(vm.state) { it.fixing.isEmpty() && requests("POST").isNotEmpty() }
        assertTrue(requests("POST").single().body.readUtf8().contains("\"confirm\":true"))
    }

    @Test fun routerRefusedShowsRouterError() = runTest(main.dispatcher) {
        val fixJson = ""","fix":{"kind":"auto","label":"Fix DNS","role":"operator"}"""
        routes["GET /api/v1/routers/s1/checks"] = ok(checks(item("dns.remote_requests", "fail", fixJson)))
        routes["POST /api/v1/routers/s1/checks/dns.remote_requests/fix"] =
            err(409, "router_refused", """{"router_error":"not enough permissions (9)"}""", msg = "refused")
        val vm = setupVm()
        await(vm.state) { it.checks is Loadable.Ready }
        vm.requestFix((vm.state.value.checks as Loadable.Ready).value.items.single())
        await(vm.state) { it.message != null }
        val m = vm.state.value.message!!
        assertTrue(m, m.contains("not enough permissions (9)"))
        assertEquals("item stays failed", CheckStatus.FAIL, (vm.state.value.checks as Loadable.Ready).value.items.single().status)
    }

    @Test fun fixRolesAreGated() {
        val ownerFix = CheckFix(kind = "auto", role = "owner")
        val opFix = CheckFix(kind = "confirm", role = "operator")
        val operator = UserInfo("3", "o", "operator")
        assertTrue(canRunFix(owner, ownerFix))
        assertFalse(canRunFix(operator, ownerFix))
        assertTrue(canRunFix(operator, opFix))
        assertFalse(canRunFix(viewer, opFix))
        assertFalse("manual fixes are copied, not run", canRunFix(owner, CheckFix(kind = "manual")))
    }

    // ---- mode ----

    @Test fun mode409AsksThenOverrides() = runTest(main.dispatcher) {
        routes["GET /api/v1/routers/s1/checks"] = ok(checks())
        sequences["PUT /api/v1/routers/s1/mode"] = mutableListOf(
            err(409, "bad_state", msg = "The router already looks like IPoE."),
            ok("""{"id":"s1","name":"Kilimani","access_mode":"hotspot"}"""),
        )
        val vm = setupVm()
        await(vm.state) { it.checks is Loadable.Ready && it.mode == "hotspot" }

        vm.setMode("ipoe_observe")
        await(vm.state) { it.modeConflict != null }
        assertEquals("The router already looks like IPoE.", vm.state.value.modeConflict!!.message)
        val first = requests("PUT").single().body.readUtf8()
        assertTrue(first, first.contains("\"override\":false") && first.contains("ipoe_observe"))

        vm.overrideMode()
        await(vm.state) { !it.modeBusy && it.modeConflict == null && requests("PUT").size == 2 }
        assertTrue(requests("PUT")[1].body.readUtf8().contains("\"override\":true"))
    }

    // ---- join command ----

    @Test fun joinCommandIsNeverCachedOrReplayed() = runTest(main.dispatcher) {
        routes["GET /api/v1/routers/s1/checks"] = ok(checks())
        sequences["POST /api/v1/routers/s1/join-command"] = mutableListOf(
            ok("""{"command":"/tool fetch one","command_nocheck":"/tool fetch one-nocheck","expires_at":"2099-01-01T00:00:00Z"}"""),
            ok("""{"command":"/tool fetch two","command_nocheck":"","expires_at":"2099-01-01T00:00:00Z"}"""),
        )
        val vm = setupVm()
        await(vm.state) { it.checks is Loadable.Ready }

        vm.getJoinCommand()
        await(vm.state) { it.join.command != null }
        assertEquals("/tool fetch one", vm.state.value.join.command!!.command)

        vm.clearJoinCommand()
        assertNull("hiding drops the secret", vm.state.value.join.command)

        vm.getJoinCommand()
        await(vm.state) { it.join.command?.command == "/tool fetch two" }
        val posts = requests("POST /api/v1/routers/s1/join-command")
        assertEquals("every show is a fresh request", 2, posts.size)
        posts.forEach { assertNull("no idempotency key so the server cannot replay it", it.getHeader("Idempotency-Key")) }
    }

    @Test fun otherMutationsStillCarryAnIdempotencyKey() = runTest(main.dispatcher) {
        routes["GET /api/v1/routers/s1/checks"] = ok(checks())
        routes["PUT /api/v1/routers/s1/mode"] = ok("""{"id":"s1","name":"Kilimani","access_mode":"ipoe"}""")
        val vm = setupVm()
        await(vm.state) { it.checks is Loadable.Ready && it.mode == "hotspot" }
        vm.setMode("ipoe")
        await(vm.state) { it.mode == "ipoe" }
        assertNotNull(requests("PUT").single().getHeader("Idempotency-Key"))
    }

    // ---- automatic setup job ----

    private fun job(state: String, error: String = "", kind: String = "migrate") =
        ok("""{"router_id":"s1","state":"$state","kind":"$kind","seconds_left":600,"next_action":{"kind":"none"},""" +
            """"steps":[{"n":1,"key":"preflight","title":"Preflight","status":"ok"}],""" +
            """"evidence":{"tunnel_up":true,"internet_ok":true,"flats_required":3,"flats_bound":3},"blockers":[],"last_error":"$error"}""")

    private fun jobVm() = SetupJobViewModel("s1", SiteSetupRepository(api), SitesRepository(api), pollMs = 10, clock = { 0L })

    private fun TestScope.settle() {
        advanceUntilIdle()
        Thread.sleep(150)
        advanceUntilIdle()
    }

    private fun stateOf(vm: SetupJobViewModel) = (vm.state.value.setup as? Loadable.Ready)?.value?.state

    @Test fun setupPollsWhileWorkingThenStopsWhenLiveWithoutAnyConfirmCall() = runTest(main.dispatcher) {
        sequences["GET /api/v1/routers/s1/setup"] = mutableListOf(job("switching"), job("verifying"), job("live"))
        val vm = jobVm()
        await(vm.state) { (it.setup as? Loadable.Ready)?.value?.state == "live" }
        assertTrue("polled while working", requests("GET /api/v1/routers/s1/setup").size >= 3)
        settle()
        val after = requests("GET /api/v1/routers/s1/setup").size
        settle()
        assertEquals("polling stopped once live", after, requests("GET /api/v1/routers/s1/setup").size)
        assertTrue("nothing is ever posted", requests("POST").isEmpty())
        assertEquals(3, (vm.state.value.setup as Loadable.Ready).value.evidence.flatsBound)
    }

    @Test fun setupStopsPollingWhenFailedAndShowsLastError() = runTest(main.dispatcher) {
        sequences["GET /api/v1/routers/s1/setup"] = mutableListOf(job("staging"), job("failed", error = "step 5: router refused"))
        val vm = jobVm()
        await(vm.state) { stateOf(vm) == "failed" }
        settle()
        val n = requests("GET /api/v1/routers/s1/setup").size
        settle()
        assertEquals(n, requests("GET /api/v1/routers/s1/setup").size)
        assertEquals("step 5: router refused", (vm.state.value.setup as Loadable.Ready).value.lastError)
    }

    @Test fun readyJobDoesNotPoll() = runTest(main.dispatcher) {
        routes["GET /api/v1/routers/s1/setup"] = job("ready")
        val vm = jobVm()
        await(vm.state) { it.setup is Loadable.Ready }
        settle()
        assertEquals(1, requests("GET /api/v1/routers/s1/setup").size)
    }

    @Test fun startMigrationNeedsTheTypedRouterName() = runTest(main.dispatcher) {
        routes["GET /api/v1/routers/s1/setup"] = job("ready")
        routes["POST /api/v1/routers/s1/setup/migrate"] = job("arming")
        val vm = jobVm()
        await(vm.state) { it.setup is Loadable.Ready && it.routerName == "Kilimani" }

        vm.setTypedName("kilimani")
        assertFalse(vm.canStart())
        vm.startMigration()
        advanceUntilIdle()
        assertTrue(requests("POST").isEmpty())

        vm.setTypedName("Kilimani")
        vm.setKeepUnknown(true)
        assertTrue(vm.canStart())
        vm.startMigration()
        await(vm.state) { stateOf(vm) == "arming" }
        val body = requests("POST").first().body.readUtf8()
        assertTrue(body, body.contains("\"confirm_name\":\"Kilimani\"") && body.contains("\"keep_unknown\":true"))
    }

    @Test fun rollbackNowPostsAndRouterRefusedMessageIsShown() = runTest(main.dispatcher) {
        routes["GET /api/v1/routers/s1/setup"] = job("verifying")
        routes["POST /api/v1/routers/s1/setup/rollback"] = err(409, "router_refused", """{"router_error":"failure: no such item"}""")
        val vm = jobVm()
        await(vm.state) { it.setup is Loadable.Ready }
        vm.rollbackNow()
        await(vm.state) { it.message != null }
        assertTrue(vm.state.value.message!!.contains("failure: no such item"))
        assertEquals(1, requests("POST /api/v1/routers/s1/setup/rollback").size)
    }
}
