package ke.co.ftw.ftnadmin.ui

import ke.co.ftw.ftnadmin.core.network.ApiClient
import ke.co.ftw.ftnadmin.core.network.MemoryTokenStore
import ke.co.ftw.ftnadmin.core.network.Session
import ke.co.ftw.ftnadmin.core.network.SessionManager
import ke.co.ftw.ftnadmin.core.network.UserInfo
import ke.co.ftw.ftnadmin.data.sites.CheckStatus
import ke.co.ftw.ftnadmin.data.sites.SiteSetupRepository
import ke.co.ftw.ftnadmin.data.sites.SitesRepository
import ke.co.ftw.ftnadmin.ui.routers.setup.RouterSetupViewModel
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
        api = ApiClient.create(server.url("/").toString(), sessions, io = main.dispatcher)
        routes["GET /api/v1/routers/s1"] = ok("""{"id":"s1","name":"Kilimani","access_mode":"ipoe"}""")
    }

    @After fun tearDown() {
        // Cancel the view models first, so no call is resumed on a Main that is about to be reset.
        main.cancelViewModels()
        server.shutdown()
    }

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

    private fun setupVm() = main.track(SetupViewModel("s1", SiteSetupRepository(api)))

    private fun item(id: String, status: String, fix: String = "") =
        """{"id":"$id","group":"network","status":"$status","title":"T $id","detail":"d"$fix}"""

    private fun checks(vararg items: String) =
        """{"site_id":"s1","generated_at":"2026-11-18T10:00:00Z","router":{"reachable":true,"version":"7.25.1"},""" +
            """"summary":{"ok":0,"warn":0,"fail":1,"unknown":0},"join_report":null,"items":[${items.joinToString(",")}]}"""

    // ---- checks ----

    @Test fun checksLoadThenPullToRefreshUsesFresh() = runTest(main.dispatcher) {
        routes["GET /api/v1/routers/s1/checks"] = ok(checks(item("dns.remote_requests", "fail")))
        val vm = setupVm()
        await(vm.state) { it.checks is Loadable.Ready }
        val first = requests("GET /api/v1/routers/s1/checks").single()
        assertNull("first load may use the server cache", first.requestUrl!!.queryParameter("fresh"))

        vm.refresh()
        await(vm.state) { !it.refreshing && requests("GET /api/v1/routers/s1/checks").size == 2 }
        assertEquals("1", requests("GET /api/v1/routers/s1/checks")[1].requestUrl!!.queryParameter("fresh"))
        val c = (vm.state.value.checks as Loadable.Ready).value
        assertEquals(CheckStatus.FAIL, c.items.single().status)
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
        // After the fix the router reports the check as ok: the fresh re-read the view model starts at once must not
        // race the assertion below with the stale "warn".
        routes["GET /api/v1/routers/s1/checks"] = ok(checks(item("traffic_flow", "ok")))

        vm.requestFix(it0)
        await(vm.state) { s -> s.fixing.isEmpty() && s.message?.contains("set interface") == true }
        val posted = requests("POST /api/v1/routers/s1/checks/traffic_flow/fix").single()
        assertFalse("auto fixes carry no confirm flag", posted.body.readUtf8().contains("confirm"))
        val c = (vm.state.value.checks as Loadable.Ready).value
        assertEquals(CheckStatus.OK, c.items.single().status)
        await(vm.state) { requests("GET /api/v1/routers/s1/checks").any { r -> r.requestUrl!!.queryParameter("fresh") == "1" } }
    }

    @Test fun confirmFixWaitsForDialogThenSendsConfirm() = runTest(main.dispatcher) {
        val fixJson = ""","fix":{"kind":"confirm","label":"Remove OLT port","confirm_text":"Learned MACs: AA","role":"owner"}"""
        routes["GET /api/v1/routers/s1/checks"] = ok(checks(item("olt.bridge_conflict", "fail", fixJson)))
        routes["POST /api/v1/routers/s1/checks/olt.bridge_conflict/fix"] = ok("""{"item":${item("olt.bridge_conflict", "ok")},"applied":[]}""")
        val vm = setupVm()
        await(vm.state) { it.checks is Loadable.Ready }
        val i = (vm.state.value.checks as Loadable.Ready).value.items.single()

        vm.requestFix(i)
        advanceUntilIdle()
        assertEquals(i.id, vm.state.value.confirmFix?.id)
        assertTrue("nothing is sent before the person taps Apply", requests("POST").isEmpty())

        vm.cancelFix()
        advanceUntilIdle()
        assertTrue(requests("POST").isEmpty())

        vm.requestFix(i)
        vm.confirmFix()
        await(vm.state) { it.fixing.isEmpty() && it.confirmFix == null && requests("POST").isNotEmpty() }
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

    // ---- removed endpoints are never called ----

    @Test fun checksScreenNeverCallsRemovedEndpoints() = runTest(main.dispatcher) {
        routes["GET /api/v1/routers/s1/checks"] = ok(checks())
        val vm = setupVm()
        await(vm.state) { it.checks is Loadable.Ready }
        vm.refresh()
        await(vm.state) { !it.refreshing }
        assertTrue(requests("PUT").isEmpty())
        assertTrue(requests("POST").isEmpty())
    }

    @Test fun repositoryHasNoModeCutoverOrMigrateCalls() {
        val names = SiteSetupRepository::class.java.declaredMethods.map { it.name }
        listOf("setMode", "runSetup", "migrate", "rollback", "cutover").forEach { n ->
            assertFalse("$n must be gone", names.any { it.startsWith(n) })
        }
    }

    // ---- v5 setup card ----

    private fun setupJson(state: String, vararg status: String, error: String = "", kind: String = "wait") =
        """{"router_id":"s1","state":"$state","join":{"state":"used"},"next_action":{"kind":"$kind","text":"Waiting for the tunnel"},""" +
            """"steps":[${status.mapIndexed { i, st -> """{"n":${i + 1},"key":"${listOf("script_fetched", "tunnel_up", "server_login", "checks_green")[i]}","title":"T${i + 1}","status":"$st"}""" }.joinToString(",")}],""" +
            """"checks":{"ok":3,"warn":0,"fail":0,"unknown":0},"blockers":[],"last_error":"$error"}"""

    private val greenSetup = setupJson("ready", "ok", "ok", "ok", "ok", kind = "none")

    private fun setupCardVm(user: UserInfo = owner) =
        main.track(RouterSetupViewModel("s1", SiteSetupRepository(api), SitesRepository(api), user, pollMs = 10))

    private fun TestScope.settle() {
        advanceUntilIdle()
        Thread.sleep(150)
        advanceUntilIdle()
    }

    private fun setupOf(vm: RouterSetupViewModel) = (vm.state.value.setup as? Loadable.Ready)?.value

    @Test fun setupPollsWhileNotGreenThenStops() = runTest(main.dispatcher) {
        sequences["GET /api/v1/routers/s1/setup"] = mutableListOf(
            ok(setupJson("waiting_router", "ok", "running", "pending", "pending")),
            ok(setupJson("online", "ok", "ok", "running", "pending")),
            ok(greenSetup),
        )
        val vm = setupCardVm()
        await(vm.state) { (it.setup as? Loadable.Ready)?.value?.state == "ready" }
        assertTrue("polled while not green", requests("GET /api/v1/routers/s1/setup").size >= 3)
        assertEquals(listOf("script_fetched", "tunnel_up", "server_login", "checks_green"), setupOf(vm)!!.steps.map { it.key })
        settle()
        val n = requests("GET /api/v1/routers/s1/setup").size
        settle()
        assertEquals("polling stopped when all green", n, requests("GET /api/v1/routers/s1/setup").size)
        assertTrue("nothing is posted by the card", requests("POST").isEmpty())
    }

    @Test fun pullToRefreshRereadsAndPauseStopsThePoll() = runTest(main.dispatcher) {
        routes["GET /api/v1/routers/s1/setup"] = ok(setupJson("waiting_router", "ok", "running", "pending", "pending"))
        val vm = setupCardVm()
        await(vm.state) { it.setup is Loadable.Ready }
        vm.pause()
        settle()
        val n = requests("GET /api/v1/routers/s1/setup").size
        vm.refresh()
        await(vm.state) { !it.refreshing && requests("GET /api/v1/routers/s1/setup").size > n }
        vm.pause()
        settle()
    }

    @Test fun failedPollKeepsTheLastViewAndMarksItStale() = runTest(main.dispatcher) {
        sequences["GET /api/v1/routers/s1/setup"] = mutableListOf(
            ok(setupJson("waiting_router", "ok", "running", "pending", "pending")),
            err(503, "router_unreachable"),
            ok(greenSetup),
        )
        val vm = setupCardVm()
        await(vm.state) { setupOf(vm)?.state == "ready" }
        assertFalse(vm.state.value.stale)
        assertTrue(vm.state.value.setup is Loadable.Ready)
    }

    // ---- bootstrap line ----

    @Test fun bootstrapLineIsOwnerOnlyNeverCachedAndHidden() = runTest(main.dispatcher) {
        routes["GET /api/v1/routers/s1/setup"] = ok(greenSetup)
        sequences["POST /api/v1/routers/s1/join-command"] = mutableListOf(
            ok("""{"command":"/tool fetch one","command_nocheck":"/tool fetch one-nocheck","expires_at":"2099-01-01T00:00:00Z"}"""),
            ok("""{"command":"/tool fetch two","command_nocheck":"","expires_at":"2099-01-01T00:00:00Z"}"""),
        )
        val viewerVm = setupCardVm(viewer)
        await(viewerVm.state) { it.setup is Loadable.Ready }
        viewerVm.getLine()
        settle()
        assertTrue("viewers cannot mint a line", requests("POST").isEmpty())

        val vm = setupCardVm()
        await(vm.state) { it.setup is Loadable.Ready }
        vm.getLine()
        await(vm.state) { it.line.command != null }
        assertEquals("/tool fetch one", vm.state.value.line.command!!.command)
        vm.hideLine()
        assertNull("hiding drops the secret", vm.state.value.line.command)
        vm.getLine()
        await(vm.state) { it.line.command?.command == "/tool fetch two" }
        val posts = requests("POST /api/v1/routers/s1/join-command")
        assertEquals(2, posts.size)
        posts.forEach { assertNull("no idempotency key so the server cannot replay it", it.getHeader("Idempotency-Key")) }
    }

    // ---- reset ----

    private fun resetVm(user: UserInfo = owner): RouterSetupViewModel {
        routes["GET /api/v1/routers/s1/setup"] = ok(greenSetup)
        return setupCardVm(user)
    }

    @Test fun resetNeedsTheTypedNameThenShowsTheNewLine() = runTest(main.dispatcher) {
        val vm = resetVm()
        routes["POST /api/v1/routers/s1/actions/reset"] = ok(
            """{"router":{"id":"s1","name":"Kilimani"},"join":{"command":"/tool fetch new","command_nocheck":"x","expires_at":"2099-01-01T00:00:00Z"}}""",
        )
        await(vm.state) { it.setup is Loadable.Ready && it.routerName == "Kilimani" }
        vm.openReset()
        vm.setTypedName("kilimani")
        assertFalse(vm.canReset())
        vm.reset()
        advanceUntilIdle()
        assertTrue(requests("POST").isEmpty())

        vm.setTypedName("Kilimani")
        assertTrue(vm.canReset())
        vm.reset()
        await(vm.state) { it.line.command != null }
        val post = requests("POST /api/v1/routers/s1/actions/reset").single()
        assertTrue(post.body.readUtf8().contains("\"confirm_name\":\"Kilimani\""))
        assertNull("the line is a secret: no replay", post.getHeader("Idempotency-Key"))
        assertEquals("/tool fetch new", vm.state.value.line.command!!.command)
        assertFalse(vm.state.value.resetDialog)
    }

    @Test fun resetWrongNameFromServerStaysInTheDialog() = runTest(main.dispatcher) {
        val vm = resetVm()
        routes["POST /api/v1/routers/s1/actions/reset"] =
            err(400, "validation_failed", """{"fields":{"confirm_name":"does not match"}}""", msg = "invalid")
        await(vm.state) { it.routerName == "Kilimani" }
        vm.openReset()
        vm.setTypedName("Kilimani")
        vm.reset()
        await(vm.state) { !it.resetBusy && it.resetError != null }
        assertTrue(vm.state.value.resetDialog)
        assertTrue(vm.state.value.resetError!!, vm.state.value.resetError!!.contains("does not match"))
        assertNull(vm.state.value.line.command)
    }

    @Test fun viewersCannotOpenReset() = runTest(main.dispatcher) {
        val vm = resetVm(viewer)
        await(vm.state) { it.routerName == "Kilimani" }
        vm.openReset()
        assertFalse(vm.state.value.resetDialog)
        vm.setTypedName("Kilimani")
        assertFalse(vm.canReset())
    }
}
