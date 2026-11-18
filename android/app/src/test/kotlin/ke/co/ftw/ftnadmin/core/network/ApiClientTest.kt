package ke.co.ftw.ftnadmin.core.network

import ke.co.ftw.ftnadmin.data.accounts.AccountsRepository
import ke.co.ftw.ftnadmin.data.auth.AuthRepository
import ke.co.ftw.ftnadmin.data.auth.DeviceInfo
import ke.co.ftw.ftnadmin.data.auth.SecondFactor
import ke.co.ftw.ftnadmin.data.dashboard.DashboardRepository
import java.util.concurrent.atomic.AtomicInteger
import kotlinx.coroutines.async
import kotlinx.coroutines.awaitAll
import kotlinx.coroutines.runBlocking
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
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
import org.junit.Assert.fail
import org.junit.Before
import org.junit.Test

class MemoryTokenStore(var session: Session? = null) : TokenStore {
    override fun load() = session

    override fun save(session: Session) {
        this.session = session
    }

    override fun clear() {
        session = null
    }
}

class ApiClientTest {
    private lateinit var server: MockWebServer
    private lateinit var store: MemoryTokenStore
    private lateinit var sessions: SessionManager
    private lateinit var api: ApiClient
    private val device = DeviceInfo("install-1", "Test Phone", "Pixel", "1.0")

    @Before fun setUp() {
        server = MockWebServer()
        server.start()
        store = MemoryTokenStore()
        sessions = SessionManager(store)
        api = ApiClient.create(server.url("/").toString(), sessions)
    }

    @After fun tearDown() {
        runCatching { server.shutdown() }
    }

    private fun signedIn(access: String = "acc-1", refresh: String = "ref-1") {
        store.session = Session(access, "2026-01-01T00:00:00Z", refresh, "2026-03-01T00:00:00Z", "dev-1", UserInfo("u1", "brian", "owner"))
        sessions.restore()
    }

    private fun tokens(access: String, refresh: String) =
        """{"access_token":"$access","access_expires_at":"2026-01-01T00:15:00Z","refresh_token":"$refresh","refresh_expires_at":"2026-03-01T00:00:00Z","device_id":"dev-1","user":{"id":"u1","username":"brian","role":"owner"}}"""

    private fun json(code: Int, body: String) =
        MockResponse().setResponseCode(code).setHeader("Content-Type", "application/json").setBody(body)

    private fun error(code: Int, errCode: String, message: String = "x", details: String = "{}") =
        json(code, """{"error":{"code":"$errCode","message":"$message","details":$details}}""")

    // ---- login ----

    @Test fun loginStoresSessionAndSendsDeviceWithoutBearer() = runBlocking {
        server.enqueue(json(200, tokens("acc-1", "ref-1")))
        AuthRepository(api, sessions, { device }).login("brian", "pw", SecondFactor(totp = "123 456"))

        val req = server.takeRequest()
        assertEquals("POST", req.method)
        assertEquals("/api/v1/auth/login", req.path)
        assertNull(req.getHeader("Authorization"))
        assertNull(req.getHeader("Idempotency-Key"))
        val body = ApiJson.parseToJsonElement(req.body.readUtf8()).jsonObject
        assertEquals("brian", body["username"]!!.jsonPrimitive.content)
        assertEquals("123456", body["totp"]!!.jsonPrimitive.content)
        assertNull(body["recovery_code"])
        val dev = body["device"]!!.jsonObject
        assertEquals("android", dev["platform"]!!.jsonPrimitive.content)
        assertEquals("install-1", dev["install_id"]!!.jsonPrimitive.content)

        assertTrue(sessions.state.value is AuthState.SignedIn)
        assertEquals("ref-1", store.session?.refreshToken)
    }

    @Test fun loginWithRecoveryCodeOmitsTotp() = runBlocking {
        server.enqueue(json(200, tokens("a", "r")))
        AuthRepository(api, sessions, { device }).login("brian", "pw", SecondFactor(recoveryCode = "abcd-efgh"))
        val body = ApiJson.parseToJsonElement(server.takeRequest().body.readUtf8()).jsonObject
        assertEquals("abcd-efgh", body["recovery_code"]!!.jsonPrimitive.content)
        assertNull(body["totp"])
    }

    @Test fun wrongPasswordIsAnErrorAndDoesNotTryToRefresh() = runBlocking {
        server.enqueue(error(401, "invalid_credentials", "Wrong username or password."))
        val e = runCatching { AuthRepository(api, sessions, { device }).login("brian", "bad", SecondFactor(totp = "000000")) }
            .exceptionOrNull() as ApiError
        assertEquals(401, e.status)
        assertEquals(ErrorCode.INVALID_CREDENTIALS, e.code)
        assertFalse(e.needsLogin)
        assertEquals(1, server.requestCount)
        assertNull(store.session)
    }

    @Test fun rateLimitCarriesRetryAfter() = runBlocking {
        server.enqueue(error(429, "rate_limited", "slow down", """{"retry_after_s":42}""").setHeader("Retry-After", "42"))
        val e = runCatching { AuthRepository(api, sessions, { device }).login("b", "p", SecondFactor(totp = "123456")) }
            .exceptionOrNull() as ApiError
        assertEquals(ErrorCode.RATE_LIMITED, e.code)
        assertEquals(42, e.retryAfterS)
    }

    // ---- refresh ----

    @Test fun expiredAccessTokenRefreshesRotatesAndRetries() = runBlocking {
        signedIn()
        server.enqueue(error(401, "token_expired"))
        server.enqueue(json(200, tokens("acc-2", "ref-2")))
        server.enqueue(json(200, """{"sites_total":3,"sites_online":2,"unmatched_payments":4}"""))

        val d = DashboardRepository(api).dashboard()
        assertEquals(3, d.sitesTotal)

        val first = server.takeRequest()
        assertEquals("Bearer acc-1", first.getHeader("Authorization"))
        val refresh = server.takeRequest()
        assertEquals("/api/v1/auth/refresh", refresh.path)
        assertNull(refresh.getHeader("Authorization"))
        assertEquals("ref-1", ApiJson.parseToJsonElement(refresh.body.readUtf8()).jsonObject["refresh_token"]!!.jsonPrimitive.content)
        val retry = server.takeRequest()
        assertEquals("Bearer acc-2", retry.getHeader("Authorization"))
        assertEquals("ref-2", store.session?.refreshToken)
        assertEquals("acc-2", store.session?.accessToken)
        assertTrue(sessions.state.value is AuthState.SignedIn)
    }

    @Test fun parallelUnauthorisedCallsShareOneRefresh() = runBlocking {
        signedIn()
        val refreshes = AtomicInteger()
        server.dispatcher = object : Dispatcher() {
            override fun dispatch(request: RecordedRequest): MockResponse = when {
                request.path == "/api/v1/auth/refresh" -> {
                    refreshes.incrementAndGet()
                    Thread.sleep(150)
                    json(200, tokens("acc-2", "ref-2"))
                }
                request.getHeader("Authorization") == "Bearer acc-2" -> json(200, """{"sites_total":1}""")
                else -> error(401, "token_expired")
            }
        }
        val repo = DashboardRepository(api)
        val results = (1..5).map { async { repo.dashboard() } }.awaitAll()
        assertTrue(results.all { it.sitesTotal == 1 })
        assertEquals("exactly one refresh for five parallel 401s", 1, refreshes.get())
    }

    @Test fun refreshTokenReuseRevokesDeviceAndSignsOut() = runBlocking {
        signedIn()
        server.enqueue(error(401, "token_expired"))
        server.enqueue(error(401, "token_revoked", "This device was revoked."))
        val e = runCatching { DashboardRepository(api).dashboard() }.exceptionOrNull() as ApiError
        assertEquals(401, e.status)
        assertEquals(2, server.requestCount)
        val s = sessions.state.value
        assertTrue(s is AuthState.SignedOut)
        assertNotNull((s as AuthState.SignedOut).reason)
        assertNull("tokens must be wiped", store.session)
        assertNull(sessions.current)
    }

    @Test fun revokedAccessTokenSignsOutWithoutRefreshing() = runBlocking {
        signedIn()
        server.enqueue(error(401, "token_revoked"))
        runCatching { DashboardRepository(api).dashboard() }
        assertEquals("no refresh attempt for a revoked device", 1, server.requestCount)
        assertTrue(sessions.state.value is AuthState.SignedOut)
        assertNull(store.session)
    }

    @Test fun refreshOutageKeepsTokens() = runBlocking {
        signedIn()
        server.enqueue(error(401, "token_expired"))
        server.enqueue(error(503, "db_unavailable"))
        val e = runCatching { DashboardRepository(api).dashboard() }.exceptionOrNull() as ApiError
        assertEquals(401, e.status)
        assertTrue("a refresh outage must not log the operator out", sessions.state.value is AuthState.SignedIn)
        assertEquals("ref-1", store.session?.refreshToken)
    }

    // ---- 409 mapping and request shape ----

    @Test fun macInUseNamesOwningFlatAndBuilding() = runBlocking {
        signedIn()
        server.enqueue(
            error(
                409, "mac_in_use", "That MAC belongs to another account.",
                """{"mac":"AA:BB:CC:DD:EE:FF","account":{"id":"acc9","code":"FTN-0042","flat":"B4","site_id":"s2","site_name":"Kileleshwa Heights"}}""",
            ),
        )
        val e = runCatching { AccountsRepository(api).changeMac("acc1", "AA:BB:CC:DD:EE:FF") }.exceptionOrNull() as ApiError
        assertEquals(409, e.status)
        assertEquals(ErrorCode.MAC_IN_USE, e.code)
        val owner = e.macOwner!!
        assertEquals("B4", owner.flat)
        assertEquals("Kileleshwa Heights", owner.siteName)
        assertEquals("FTN-0042", owner.code)
        assertEquals("AA:BB:CC:DD:EE:FF", owner.mac)

        val req = server.takeRequest()
        assertEquals("PUT", req.method)
        assertEquals("/api/v1/accounts/acc1/mac", req.path)
        assertEquals("Bearer acc-1", req.getHeader("Authorization"))
        assertNotNull("mutating calls carry an idempotency key", req.getHeader("Idempotency-Key"))
    }

    @Test fun otherConflictsKeepTheirCode() = runBlocking {
        signedIn()
        server.enqueue(error(409, "lower_plan_active", "A better plan is running."))
        val e = runCatching { AccountsRepository(api).extend("a1", "pkg1") }.exceptionOrNull() as ApiError
        assertEquals(ErrorCode.LOWER_PLAN_ACTIVE, e.code)
        assertNull(e.macOwner)
    }

    @Test fun validationFieldsAreExposed() = runBlocking {
        signedIn()
        server.enqueue(error(400, "validation_failed", "Check the form.", """{"fields":{"mac":"not a MAC address"}}"""))
        val e = runCatching { AccountsRepository(api).changeMac("a1", "zz") }.exceptionOrNull() as ApiError
        assertEquals("not a MAC address", e.fieldErrors["mac"])
    }

    @Test fun getsAreNotIdempotencyKeyed() = runBlocking {
        signedIn()
        server.enqueue(json(200, """{"items":[],"next_cursor":null}"""))
        AccountsRepository(api).list(siteId = "s1", state = "active", query = "b 4")
        val req = server.takeRequest()
        assertNull(req.getHeader("Idempotency-Key"))
        assertTrue(req.path!!, req.path!!.startsWith("/api/v1/accounts?"))
        assertEquals("s1", req.requestUrl!!.queryParameter("site"))
        assertEquals("active", req.requestUrl!!.queryParameter("state"))
        assertEquals("b 4", req.requestUrl!!.queryParameter("q"))
    }

    @Test fun idsArePercentEncoded() = runBlocking {
        signedIn()
        server.enqueue(json(200, """{"key":"k/1"}"""))
        ke.co.ftw.ftnadmin.data.payments.PaymentsRepository(api).assign("mpesa/AB 1", "acc", "pkg", null)
        assertEquals("/api/v1/payments/mpesa%2FAB%201/assign", server.takeRequest().path)
    }

    @Test fun unreachableServerIsANetworkError() = runBlocking {
        signedIn()
        server.shutdown()
        try {
            DashboardRepository(api).dashboard()
            fail("expected ApiError")
        } catch (e: ApiError) {
            assertTrue(e.isNetwork)
            assertEquals(ErrorCode.NETWORK, e.code)
        }
        assertTrue(sessions.state.value is AuthState.SignedIn)
    }

    @Test fun nonJsonErrorBodyStillMaps() = runBlocking {
        signedIn()
        server.enqueue(MockResponse().setResponseCode(502).setBody("<html>Bad gateway</html>"))
        val e = runCatching { DashboardRepository(api).dashboard() }.exceptionOrNull() as ApiError
        assertEquals(502, e.status)
        assertEquals(ErrorCode.UNKNOWN, e.code)
    }
}
