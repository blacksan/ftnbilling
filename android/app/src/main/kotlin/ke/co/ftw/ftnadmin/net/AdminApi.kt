package ke.co.ftw.ftnadmin.net

import ke.co.ftw.ftnadmin.auth.CredentialStore
import ke.co.ftw.ftnadmin.auth.Credentials
import ke.co.ftw.ftnadmin.tunnel.TunnelAccess
import ke.co.ftw.ftnadmin.tunnel.TunnelState
import ke.co.ftw.ftnadmin.web.ContentDisposition
import java.io.File
import java.io.IOException
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext

/** A file received from an export, waiting for the owner to choose where to save it. */
class ExportFile(val file: File, val fileName: String, val mimeType: String)

/**
 * Calls the admin server through the VPN network. Every call refuses unless the tunnel is [TunnelState.Up],
 * so stored credentials can never be sent to whatever 10.98.0.1 resolves to on the local network.
 */
class AdminApi(
    private val tunnel: TunnelAccess,
    private val credentials: CredentialStore,
) {
    private fun isUp() = tunnel.state.value is TunnelState.Up

    suspend fun health(): Fetch<ServerHealth> = withContext(Dispatchers.IO) {
        if (!isUp()) return@withContext Fetch.Failed("The tunnel is not connected.")
        try {
            val r = AdminHttp.get("${AdminHttp.ORIGIN}/healthz", tunnel.vpnNetwork())
            val parsed = if (r.code == 200) JsonParsers.parseHealth(r.body) else null
            if (parsed != null) Fetch.Ok(parsed) else Fetch.Failed("Unexpected answer from the server (HTTP ${r.code}).")
        } catch (e: IOException) {
            Fetch.Failed(e.message ?: "Network error")
        }
    }

    suspend fun summary(): Fetch<FleetSummary> = withContext(Dispatchers.IO) {
        if (!isUp()) return@withContext Fetch.Failed("The tunnel is not connected.")
        val creds = credentials.current() ?: return@withContext Fetch.AuthNeeded
        try {
            val r = AdminHttp.get(
                "${AdminHttp.ORIGIN}/admin/api/summary",
                tunnel.vpnNetwork(),
                mapOf("Authorization" to AdminHttp.basicAuth(creds), "Accept" to "application/json"),
            )
            when (r.code) {
                200 -> JsonParsers.parseSummary(r.body)?.let { Fetch.Ok(it) }
                    ?: Fetch.Failed("The fleet summary could not be read.")
                401 -> Fetch.AuthNeeded
                404 -> Fetch.ServerTooOld
                429 -> Fetch.Failed("Too many sign-in attempts. Try again later.")
                else -> Fetch.Failed("The server answered HTTP ${r.code}.")
            }
        } catch (e: IOException) {
            Fetch.Failed(e.message ?: "Network error")
        }
    }

    /**
     * Downloads an export inside the app, sending the Authorization header and the WebView's cookies
     * (the `ftn_site` cookie selects the building). Saved to [dir]; the caller moves it to its final place.
     */
    @Throws(IOException::class)
    suspend fun download(url: String, creds: Credentials, cookies: String?, dir: File): ExportFile = withContext(Dispatchers.IO) {
        if (!isUp()) throw IOException("The tunnel is not connected.")
        val headers = buildMap {
            put("Authorization", AdminHttp.basicAuth(creds))
            if (!cookies.isNullOrEmpty()) put("Cookie", cookies)
        }
        val conn = AdminHttp.open(url, tunnel.vpnNetwork(), headers, timeoutMs = 20_000)
        try {
            val code = conn.responseCode
            if (code != 200) throw IOException("The server answered HTTP $code.")
            val name = ContentDisposition.fileName(conn.getHeaderField("Content-Disposition"))
            val mime = conn.contentType?.substringBefore(';')?.trim()?.ifEmpty { null } ?: "text/csv"
            if (!dir.isDirectory && !dir.mkdirs()) throw IOException("Cannot create the export folder.")
            val file = File.createTempFile("export", ".tmp", dir)
            try {
                conn.inputStream.use { AdminHttp.copyLimited(it, file) }
            } catch (e: IOException) {
                file.delete()
                throw e
            }
            ExportFile(file, name, mime)
        } finally {
            conn.disconnect()
        }
    }
}
