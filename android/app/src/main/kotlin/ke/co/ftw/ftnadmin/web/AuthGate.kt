package ke.co.ftw.ftnadmin.web

import ke.co.ftw.ftnadmin.auth.Credentials

/** Answer to one HTTP auth challenge. */
sealed interface AuthOutcome {
    data class Proceed(val credentials: Credentials) : AuthOutcome

    /** Do not answer: the tunnel is down or the challenge is from another host. */
    data object Cancel : AuthOutcome

    /** Ask the owner natively. */
    data object Prompt : AuthOutcome
}

/**
 * Decides how to answer Basic-auth challenges. Rules:
 * - credentials are never released unless the tunnel is up and the host is the admin host;
 * - stored credentials are offered at most once per navigation, so a changed password or a logout
 *   (the server always answers 401 there) cannot loop and trip the login limiter (5 failures / 15 min);
 * - a login typed by the owner is used first, and only saved after the page loaded without a 401.
 */
class AuthGate(
    private val adminHost: String,
    private val isTunnelUp: () -> Boolean,
    private val storedCredentials: () -> Credentials?,
) {
    private var attemptedThisNavigation = false
    private var candidate: Credentials? = null
    private var candidateUsed = false

    /** Credentials the WebView last answered with, for in-app downloads. Only valid while the tunnel is up. */
    @Volatile
    private var lastUsed: Credentials? = null

    fun onNavigationStart() {
        attemptedThisNavigation = false
    }

    fun onAuthRequest(host: String): AuthOutcome {
        if (!isTunnelUp() || !host.equals(adminHost, ignoreCase = true)) return AuthOutcome.Cancel
        if (attemptedThisNavigation) return AuthOutcome.Prompt
        val creds = candidate ?: storedCredentials()
        if (creds == null) return AuthOutcome.Prompt
        attemptedThisNavigation = true
        if (creds === candidate) candidateUsed = true
        lastUsed = creds
        return AuthOutcome.Proceed(creds)
    }

    /** The owner typed a login. It is offered first on the next navigation. */
    fun offer(credentials: Credentials) {
        candidate = credentials
        candidateUsed = false
        attemptedThisNavigation = false
    }

    /**
     * A main-frame page finished. Returns the offered credentials if they were used and the page did
     * not come back with 401, i.e. they are known good.
     */
    fun onPageFinished(unauthorized: Boolean): Credentials? {
        val c = candidate ?: return null
        if (!candidateUsed) return null
        candidate = null
        candidateUsed = false
        if (unauthorized) {
            lastUsed = null
            return null
        }
        return c
    }

    /** Credentials for in-app downloads; null when the tunnel is down. */
    fun credentialsForDownload(): Credentials? = if (isTunnelUp()) lastUsed ?: storedCredentials() else null

    fun reset() {
        attemptedThisNavigation = false
        candidate = null
        candidateUsed = false
        lastUsed = null
    }
}
