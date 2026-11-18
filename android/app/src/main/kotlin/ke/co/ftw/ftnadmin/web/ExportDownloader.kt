package ke.co.ftw.ftnadmin.web

import android.content.Context
import android.net.Uri
import android.webkit.CookieManager
import ke.co.ftw.ftnadmin.auth.Credentials
import ke.co.ftw.ftnadmin.net.AdminApi
import ke.co.ftw.ftnadmin.net.ExportFile
import java.io.File
import java.io.IOException
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext

/**
 * CSV exports are fetched inside the app (not with DownloadManager, which would need the Basic
 * credentials handed to another process, and cannot send the `ftn_site` cookie reliably). The
 * file lands in the cache, then the owner picks where to save it.
 */
class ExportDownloader(
    context: Context,
    private val api: AdminApi,
    private val policy: UrlPolicy,
) {
    private val appContext = context.applicationContext
    private val dir = File(appContext.cacheDir, "exports")

    /** Removes leftovers from an earlier session. */
    fun clean() {
        dir.listFiles()?.forEach { it.delete() }
    }

    @Throws(IOException::class)
    suspend fun fetch(url: String, credentials: Credentials?): ExportFile {
        if (policy.decide(url) != UrlDecision.Allow) throw IOException("Only exports from the admin server can be saved.")
        if (credentials == null) throw IOException("Sign in first, then retry the export.")
        val cookies = CookieManager.getInstance().getCookie(url)
        return api.download(url, credentials, cookies, dir)
    }

    /** Copies [export] to the document the owner chose, then deletes the cache copy either way. */
    @Throws(IOException::class)
    suspend fun save(export: ExportFile, destination: Uri?) = withContext(Dispatchers.IO) {
        try {
            if (destination == null) return@withContext
            val out = appContext.contentResolver.openOutputStream(destination) ?: throw IOException("Cannot write there.")
            out.use { sink -> export.file.inputStream().use { it.copyTo(sink) } }
        } finally {
            export.file.delete()
        }
    }
}
