package ke.co.ftw.ftnadmin.web

import java.net.URI
import java.net.URISyntaxException

/** What the WebView should do with a URL it is about to load. */
sealed interface UrlDecision {
    /** Same origin: load it in the WebView. */
    data object Allow : UrlDecision

    /** `/admin/logout`: handled natively (lock and disconnect). */
    data object Logout : UrlDecision

    /** Ordinary https link: hand to the system browser. */
    data object OpenExternal : UrlDecision

    data object Block : UrlDecision
}

/** Pure JVM so it can be unit tested. The admin origin is fixed at [scheme]://[host]:[port]. */
class UrlPolicy(
    private val host: String,
    private val port: Int,
    private val scheme: String = "http",
) {
    val origin: String get() = "$scheme://$host:$port"

    fun decide(url: String): UrlDecision {
        val uri = parse(url) ?: return UrlDecision.Block
        if (isAdminOrigin(uri)) {
            val path = uri.path.orEmpty()
            return if (path.trimEnd('/') == LOGOUT_PATH) UrlDecision.Logout else UrlDecision.Allow
        }
        val s = uri.scheme?.lowercase()
        // Only plain https links leave the app. javascript:, file:, intent:, content:, http to other hosts: blocked.
        return if (s == "https" && !uri.host.isNullOrEmpty() && uri.userInfo == null) UrlDecision.OpenExternal else UrlDecision.Block
    }

    /** True for requests the WebView may make: the admin origin, or inert `about:` pages. */
    fun allowsRequest(url: String): Boolean {
        val uri = parse(url) ?: return false
        return isAdminOrigin(uri) || uri.scheme.equals("about", ignoreCase = true)
    }

    /** Builds an admin URL from an in-app path such as `/admin/sites/view?id=x`; null if it is not an admin path. */
    fun adminUrl(path: String?): String? {
        if (path.isNullOrEmpty()) return "$origin/admin"
        if (!path.startsWith("/admin") || path.startsWith("//")) return null
        val url = origin + path
        val uri = parse(url) ?: return null
        return if (isAdminOrigin(uri) && uri.path.startsWith("/admin")) url else null
    }

    private fun isAdminOrigin(uri: URI): Boolean {
        if (!uri.scheme.equals(scheme, ignoreCase = true)) return false
        if (uri.userInfo != null) return false
        if (!uri.host.equals(host, ignoreCase = true)) return false
        val p = if (uri.port == -1) 80 else uri.port
        return p == port
    }

    private fun parse(url: String): URI? = try {
        URI(url.trim())
    } catch (_: URISyntaxException) {
        null
    }

    companion object {
        const val LOGOUT_PATH = "/admin/logout"
    }
}
