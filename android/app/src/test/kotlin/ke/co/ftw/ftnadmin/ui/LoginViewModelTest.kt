package ke.co.ftw.ftnadmin.ui

import ke.co.ftw.ftnadmin.core.network.ApiClient
import ke.co.ftw.ftnadmin.core.network.AuthState
import ke.co.ftw.ftnadmin.core.network.MemoryTokenStore
import ke.co.ftw.ftnadmin.core.network.SessionManager
import ke.co.ftw.ftnadmin.data.auth.AuthRepository
import ke.co.ftw.ftnadmin.data.auth.DeviceInfo
import ke.co.ftw.ftnadmin.ui.login.FactorMode
import ke.co.ftw.ftnadmin.ui.login.LoginViewModel
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.test.advanceUntilIdle
import kotlinx.coroutines.test.TestScope
import kotlinx.coroutines.test.runTest
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Rule
import org.junit.Test

@OptIn(ExperimentalCoroutinesApi::class)
class LoginViewModelTest {
    @get:Rule val main = MainDispatcherRule()
    private lateinit var server: MockWebServer
    private lateinit var sessions: SessionManager
    private lateinit var vm: LoginViewModel

    @Before fun setUp() {
        server = MockWebServer().also { it.start() }
        sessions = SessionManager(MemoryTokenStore())
        val api = ApiClient.create(server.url("/").toString(), sessions)
        vm = LoginViewModel(AuthRepository(api, sessions, { DeviceInfo("i", "n", "m", "1") }))
    }

    @After fun tearDown() = server.shutdown()

    /** The network runs on real threads, so poll until the view model stops being busy. */
    private fun TestScope.settle() {
        advanceUntilIdle()
        val end = System.currentTimeMillis() + 5_000
        while (vm.state.value.busy && System.currentTimeMillis() < end) {
            Thread.sleep(20)
            advanceUntilIdle()
        }
    }

    private fun fill(totp: String = "123456") {
        vm.onUsername("brian")
        vm.onPassword("pw")
        vm.onTotp(totp)
    }

    private fun err(code: Int, c: String) = MockResponse().setResponseCode(code).setBody("""{"error":{"code":"$c","message":"m"}}""")

    @Test fun submitNeedsAllFields() {
        assertFalse(vm.state.value.canSubmit)
        fill("12")
        assertFalse("TOTP must be 6 digits", vm.state.value.canSubmit)
        vm.onTotp("12a3456 ")
        assertEquals("123456", vm.state.value.totp)
        assertTrue(vm.state.value.canSubmit)
    }

    @Test fun recoveryModeNeedsACodeInstead() {
        vm.onUsername("b")
        vm.onPassword("p")
        vm.toggleMode()
        assertEquals(FactorMode.Recovery, vm.state.value.mode)
        assertFalse(vm.state.value.canSubmit)
        vm.onRecovery("aaaa-bbbb")
        assertTrue(vm.state.value.canSubmit)
    }

    @Test fun successSignsInAndClearsSecrets() = runTest(main.dispatcher) {
        server.enqueue(
            MockResponse().setBody(
                """{"access_token":"a","refresh_token":"r","device_id":"d","user":{"id":"1","username":"brian","role":"owner"}}""",
            ),
        )
        fill()
        vm.submit()
        assertTrue(vm.state.value.busy)
        settle()
        assertTrue(sessions.state.value is AuthState.SignedIn)
        assertEquals("", vm.state.value.password)
        assertEquals("", vm.state.value.totp)
    }

    @Test fun wrongTotpShowsMessageAndClearsCode() = runTest(main.dispatcher) {
        server.enqueue(err(401, "totp_invalid"))
        fill()
        vm.submit()
        settle()
        val s = vm.state.value
        assertFalse(s.busy)
        assertEquals("", s.totp)
        assertNotNull(s.error)
        assertTrue(s.error!!.contains("code"))
        assertTrue(sessions.state.value !is AuthState.SignedIn)
    }

    @Test fun enrolRequiredExplainsAndOffersSetupCode() = runTest(main.dispatcher) {
        server.enqueue(err(403, "totp_enrol_required"))
        fill()
        vm.submit()
        settle()
        assertTrue(vm.state.value.enrolRequired)
        assertTrue(vm.state.value.showSetupCode)
    }
}
