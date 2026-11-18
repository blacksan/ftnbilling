package ke.co.ftw.ftnadmin.web

import android.annotation.SuppressLint
import android.content.Context
import android.net.Uri
import android.print.PrintAttributes
import android.print.PrintManager
import android.view.View
import android.webkit.CookieManager
import android.webkit.WebSettings
import android.webkit.WebStorage
import android.webkit.WebView
import android.webkit.WebViewDatabase
import ke.co.ftw.ftnadmin.BuildConfig
import ke.co.ftw.ftnadmin.auth.CredentialStore
import ke.co.ftw.ftnadmin.auth.Credentials
import ke.co.ftw.ftnadmin.net.ExportFile
import java.io.IOException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch

/** Native panels drawn over the page. */
sealed interface WebOverlay {
    data object None : WebOverlay
    data object SignIn : WebOverlay
    data class LockedOut(val retryAfterSec: Int?) : WebOverlay
    data object NotOnAdminNetwork : WebOverlay
    data class LoadError(val message: String) : WebOverlay
}

/**
 * Owns the one WebView instance of the admin screen and exposes its state to Compose.
 * Security posture: no JS bridge, no file or content access, no mixed content, no Safe Browsing
 * lookups (the page is a private address), and nothing but the admin origin is ever contacted.
 */
class AdminWebController(
    context: Context,
    private val scope: CoroutineScope,
    private val policy: UrlPolicy,
    private val gate: AuthGate,
    private val credentials: CredentialStore,
    private val downloader: ExportDownloader,
    private val onLogoutRequested: () -> Unit,
    private val onExternal: (Uri) -> Unit,
) : AdminWebEvents, AdminChromeEvents {
    private val appContext = context.applicationContext
    private var webView: WebView? = null
    private var rememberOnSuccess = false

    private var client: AdminWebViewClient? = null
    private val _overlay = MutableStateFlow<WebOverlay>(WebOverlay.None)
    val overlay: StateFlow<WebOverlay> = _overlay.asStateFlow()
    private val _progress = MutableStateFlow(0)
    val progress: StateFlow<Int> = _progress.asStateFlow()
    private val _canGoBack = MutableStateFlow(false)
    val canGoBack: StateFlow<Boolean> = _canGoBack.asStateFlow()
    private val _jsDialog = MutableStateFlow<JsDialogRequest?>(null)
    val jsDialog: StateFlow<JsDialogRequest?> = _jsDialog.asStateFlow()
    private val _pendingExport = MutableStateFlow<ExportFile?>(null)
    val pendingExport: StateFlow<ExportFile?> = _pendingExport.asStateFlow()
    private val _notice = MutableStateFlow<String?>(null)
    val notice: StateFlow<String?> = _notice.asStateFlow()

    /** Bumped when the renderer died; Compose recreates the WebView when it changes. */
    private val _generation = MutableStateFlow(0)
    val generation: StateFlow<Int> = _generation.asStateFlow()

    init {
        downloader.clean()
    }

    fun defaultUsername(): String = credentials.savedUsername() ?: "admin"

    @SuppressLint("SetJavaScriptEnabled")
    fun createWebView(context: Context, url: String): WebView {
        val wv = WebView(context)
        wv.settings.apply {
            javaScriptEnabled = true
            domStorageEnabled = true
            allowFileAccess = false
            allowContentAccess = false
            setGeolocationEnabled(false)
            safeBrowsingEnabled = false
            mixedContentMode = WebSettings.MIXED_CONTENT_NEVER_ALLOW
            setSupportMultipleWindows(false)
            cacheMode = WebSettings.LOAD_NO_CACHE
            setSupportZoom(true)
            builtInZoomControls = true
            displayZoomControls = false
            userAgentString = "$userAgentString FTNAdmin/${BuildConfig.VERSION_NAME}"
        }
        wv.importantForAutofill = View.IMPORTANT_FOR_AUTOFILL_NO_EXCLUDE_DESCENDANTS
        WebView.setWebContentsDebuggingEnabled(BuildConfig.DEBUG)
        CookieManager.getInstance().apply {
            setAcceptCookie(true)
            setAcceptThirdPartyCookies(wv, false)
        }
        wv.webViewClient = AdminWebViewClient(policy, gate, this).also { client = it }
        wv.webChromeClient = AdminChromeClient(this)
        wv.setDownloadListener { downloadUrl, _, _, _, _ -> onDownload(downloadUrl) }
        webView = wv
        wv.loadUrl(url)
        return wv
    }

    /** Stops everything and forgets session state: the tunnel went down, the screen closed or the app locked. */
    fun destroyWebView(wv: WebView) {
        wv.stopLoading()
        wv.loadUrl("about:blank")
        @Suppress("DEPRECATION")
        WebViewDatabase.getInstance(appContext).clearHttpAuthUsernamePassword()
        CookieManager.getInstance().flush()
        wv.destroy()
        if (webView === wv) {
            webView = null
            client = null
        }
        _jsDialog.value?.cancel()
        _jsDialog.value = null
        gate.reset()
    }

    fun reload() {
        client?.beginNavigation()
        gate.onNavigationStart()
        webView?.reload()
    }

    fun goBack() {
        client?.beginNavigation()
        webView?.goBack()
    }

    fun loadUrl(url: String) {
        client?.beginNavigation()
        webView?.loadUrl(url)
    }

    fun print(context: Context) {
        val wv = webView ?: return
        val manager = context.getSystemService(Context.PRINT_SERVICE) as PrintManager
        manager.print("FTN Admin", wv.createPrintDocumentAdapter("FTN Admin"), PrintAttributes.Builder().build())
    }

    /** The owner typed a login in the native dialog; try it on a fresh navigation. */
    fun submitCredentials(username: String, password: String, remember: Boolean) {
        rememberOnSuccess = remember
        gate.offer(Credentials(username.trim(), password))
        _overlay.value = WebOverlay.None
        client?.beginNavigation()
        webView?.reload()
    }

    fun dismissJsDialog(confirmed: Boolean) {
        val d = _jsDialog.value ?: return
        _jsDialog.value = null
        if (confirmed) d.confirm() else d.cancel()
    }

    fun consumeNotice() {
        _notice.value = null
    }

    fun onExportLocationChosen(destination: Uri?) {
        val export = _pendingExport.value ?: return
        _pendingExport.value = null
        scope.launch {
            _notice.value = try {
                downloader.save(export, destination)
                if (destination != null) "Saved ${export.fileName}" else null
            } catch (e: IOException) {
                "Could not save the file: ${e.message}"
            }
        }
    }

    private fun onDownload(url: String) {
        scope.launch {
            try {
                _pendingExport.value = downloader.fetch(url, gate.credentialsForDownload())
            } catch (e: IOException) {
                _notice.value = "Export failed: ${e.message}"
            }
        }
    }

    // AdminWebEvents

    override fun onLogout() {
        onLogoutRequested()
    }

    override fun onExternalLink(uri: Uri) {
        onExternal(uri)
    }

    override fun onAuthPrompt() {
        _overlay.value = WebOverlay.SignIn
    }

    override fun onLockedOut(retryAfterSec: Int?) {
        _overlay.value = WebOverlay.LockedOut(retryAfterSec)
    }

    override fun onNotOnAdminNetwork() {
        _overlay.value = WebOverlay.NotOnAdminNetwork
    }

    override fun onLoadError(message: String) {
        _overlay.value = WebOverlay.LoadError(message)
    }

    override fun onRenderProcessGone() {
        _generation.value += 1
    }

    override fun onNavigationStarted() {
        _overlay.value = WebOverlay.None
    }

    override fun onPageFinished(unauthorized: Boolean) {
        val good = gate.onPageFinished(unauthorized) ?: return
        val remember = rememberOnSuccess
        rememberOnSuccess = false
        scope.launch(Dispatchers.IO) {
            if (remember) credentials.save(good) else credentials.rememberForSession(good)
        }
    }

    override fun onHistoryChanged(canGoBack: Boolean) {
        _canGoBack.value = canGoBack
    }

    // AdminChromeEvents

    override fun onJsDialog(request: JsDialogRequest) {
        _jsDialog.value?.cancel()
        _jsDialog.value = request
    }

    override fun onProgress(percent: Int) {
        _progress.value = percent
    }

    companion object {
        /** Wipes everything the WebView keeps on disk. */
        fun clearAllWebData(context: Context) {
            CookieManager.getInstance().removeAllCookies(null)
            CookieManager.getInstance().flush()
            WebStorage.getInstance().deleteAllData()
            @Suppress("DEPRECATION")
            WebViewDatabase.getInstance(context).clearHttpAuthUsernamePassword()
            context.cacheDir.deleteRecursively()
        }
    }
}
