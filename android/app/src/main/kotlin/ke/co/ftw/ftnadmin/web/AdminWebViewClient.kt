package ke.co.ftw.ftnadmin.web

import android.graphics.Bitmap
import android.net.Uri
import android.net.http.SslError
import android.webkit.HttpAuthHandler
import android.webkit.RenderProcessGoneDetail
import android.webkit.SslErrorHandler
import android.webkit.WebResourceError
import android.webkit.WebResourceRequest
import android.webkit.WebResourceResponse
import android.webkit.WebView
import android.webkit.WebViewClient
import java.io.ByteArrayInputStream

/** Everything the WebView reports back to the native side. */
interface AdminWebEvents {
    fun onLogout()
    fun onExternalLink(uri: Uri)
    fun onAuthPrompt()
    fun onLockedOut(retryAfterSec: Int?)
    fun onNotOnAdminNetwork()
    fun onLoadError(message: String)
    fun onRenderProcessGone()
    fun onNavigationStarted()
    fun onPageFinished(unauthorized: Boolean)
    fun onHistoryChanged(canGoBack: Boolean)
}

class AdminWebViewClient(
    private val policy: UrlPolicy,
    private val gate: AuthGate,
    private val events: AdminWebEvents,
) : WebViewClient() {
    private var unauthorized = false

    override fun shouldOverrideUrlLoading(view: WebView, request: WebResourceRequest): Boolean =
        when (policy.decide(request.url.toString())) {
            UrlDecision.Allow -> false
            UrlDecision.Logout -> {
                events.onLogout()
                true
            }
            UrlDecision.OpenExternal -> {
                events.onExternalLink(request.url)
                true
            }
            UrlDecision.Block -> true
        }

    /** Nothing but the admin host may be contacted, whatever a page asks for. */
    override fun shouldInterceptRequest(view: WebView, request: WebResourceRequest): WebResourceResponse? =
        if (policy.allowsRequest(request.url.toString())) {
            null
        } else {
            WebResourceResponse("text/plain", "utf-8", 403, "Forbidden", emptyMap(), ByteArrayInputStream(ByteArray(0)))
        }

    override fun onPageStarted(view: WebView, url: String?, favicon: Bitmap?) {
        unauthorized = false
        gate.onNavigationStart()
        events.onNavigationStarted()
    }

    override fun onPageFinished(view: WebView, url: String?) {
        events.onPageFinished(unauthorized)
    }

    override fun doUpdateVisitedHistory(view: WebView, url: String?, isReload: Boolean) {
        events.onHistoryChanged(view.canGoBack())
    }

    override fun onReceivedHttpAuthRequest(view: WebView, handler: HttpAuthHandler, host: String, realm: String?) {
        when (val outcome = gate.onAuthRequest(host)) {
            is AuthOutcome.Proceed -> handler.proceed(outcome.credentials.username, outcome.credentials.password)
            AuthOutcome.Cancel -> handler.cancel()
            AuthOutcome.Prompt -> {
                handler.cancel()
                unauthorized = true
                events.onAuthPrompt()
            }
        }
    }

    override fun onReceivedHttpError(view: WebView, request: WebResourceRequest, errorResponse: WebResourceResponse) {
        if (!request.isForMainFrame) return
        when (errorResponse.statusCode) {
            401 -> {
                unauthorized = true
                events.onAuthPrompt()
            }
            429 -> {
                val retry = errorResponse.responseHeaders.entries
                    .firstOrNull { it.key.equals("Retry-After", ignoreCase = true) }?.value?.trim()?.toIntOrNull()
                events.onLockedOut(retry)
            }
            404 -> if (request.url.path.orEmpty().trimEnd('/') == "/admin") events.onNotOnAdminNetwork()
        }
    }

    override fun onReceivedError(view: WebView, request: WebResourceRequest, error: WebResourceError) {
        if (request.isForMainFrame) events.onLoadError(error.description?.toString().orEmpty().ifEmpty { "Page could not be loaded" })
    }

    override fun onReceivedSslError(view: WebView, handler: SslErrorHandler, error: SslError) {
        handler.cancel()
    }

    override fun onRenderProcessGone(view: WebView, detail: RenderProcessGoneDetail): Boolean {
        events.onRenderProcessGone()
        return true
    }
}
