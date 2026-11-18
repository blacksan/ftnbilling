package ke.co.ftw.ftnadmin.web

import android.webkit.JsResult
import android.webkit.WebChromeClient
import android.webkit.WebView

/** A JavaScript alert or confirm waiting for the owner. Must be answered exactly once. */
class JsDialogRequest(val message: String, val isConfirm: Boolean, private val result: JsResult) {
    private var answered = false

    fun confirm() = answer { result.confirm() }

    fun cancel() = answer { result.cancel() }

    private fun answer(action: () -> Unit) {
        if (answered) return
        answered = true
        action()
    }
}

interface AdminChromeEvents {
    fun onJsDialog(request: JsDialogRequest)
    fun onProgress(percent: Int)
}

/**
 * The templates call `confirm(...)` before destructive actions; without a WebChromeClient it silently
 * returns false, so the buttons would do nothing.
 */
class AdminChromeClient(private val events: AdminChromeEvents) : WebChromeClient() {
    override fun onJsAlert(view: WebView, url: String?, message: String?, result: JsResult): Boolean {
        events.onJsDialog(JsDialogRequest(message.orEmpty(), isConfirm = false, result = result))
        return true
    }

    override fun onJsConfirm(view: WebView, url: String?, message: String?, result: JsResult): Boolean {
        events.onJsDialog(JsDialogRequest(message.orEmpty(), isConfirm = true, result = result))
        return true
    }

    override fun onProgressChanged(view: WebView, newProgress: Int) {
        events.onProgress(newProgress)
    }
}
