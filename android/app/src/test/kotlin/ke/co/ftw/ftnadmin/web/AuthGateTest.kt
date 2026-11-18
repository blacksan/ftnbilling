package ke.co.ftw.ftnadmin.web

import ke.co.ftw.ftnadmin.auth.Credentials
import org.junit.Test
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue

class AuthGateTest {
    private val stored = Credentials("admin", "old-password")
    private var up = true
    private var saved: Credentials? = stored
    private val gate = AuthGate("10.98.0.1", { up }, { saved })

    @Test
    fun answersOncePerNavigationThenPrompts() {
        gate.onNavigationStart()
        assertEquals(AuthOutcome.Proceed(stored), gate.onAuthRequest("10.98.0.1"))
        // The server rejected it (password changed on the web): must not loop.
        assertEquals(AuthOutcome.Prompt, gate.onAuthRequest("10.98.0.1"))
        assertEquals(AuthOutcome.Prompt, gate.onAuthRequest("10.98.0.1"))
    }

    @Test
    fun newNavigationGetsANewAttempt() {
        gate.onNavigationStart()
        gate.onAuthRequest("10.98.0.1")
        gate.onNavigationStart()
        assertEquals(AuthOutcome.Proceed(stored), gate.onAuthRequest("10.98.0.1"))
    }

    @Test
    fun neverAnswersWhileTheTunnelIsDown() {
        up = false
        gate.onNavigationStart()
        assertEquals(AuthOutcome.Cancel, gate.onAuthRequest("10.98.0.1"))
        up = true
        assertEquals(AuthOutcome.Proceed(stored), gate.onAuthRequest("10.98.0.1"))
    }

    @Test
    fun neverAnswersForAnotherHost() {
        gate.onNavigationStart()
        assertEquals(AuthOutcome.Cancel, gate.onAuthRequest("evil.example"))
    }

    @Test
    fun promptsWhenNothingIsStored() {
        saved = null
        gate.onNavigationStart()
        assertEquals(AuthOutcome.Prompt, gate.onAuthRequest("10.98.0.1"))
    }

    @Test
    fun offeredCredentialsAreUsedFirstAndConfirmedAfterASuccessfulPage() {
        val typed = Credentials("admin", "new-password")
        gate.offer(typed)
        gate.onNavigationStart()
        assertEquals(AuthOutcome.Proceed(typed), gate.onAuthRequest("10.98.0.1"))
        assertEquals(typed, gate.onPageFinished(unauthorized = false))
        assertNull(gate.onPageFinished(unauthorized = false))
    }

    @Test
    fun offeredCredentialsAreNotConfirmedWhenRejected() {
        val typed = Credentials("admin", "wrong")
        gate.offer(typed)
        gate.onNavigationStart()
        gate.onAuthRequest("10.98.0.1")
        assertNull(gate.onPageFinished(unauthorized = true))
        assertTrue(gate.onAuthRequest("10.98.0.1") == AuthOutcome.Prompt)
    }

    @Test
    fun downloadsGetCredentialsOnlyWhileUp() {
        assertEquals(stored, gate.credentialsForDownload())
        up = false
        assertNull(gate.credentialsForDownload())
    }
}
