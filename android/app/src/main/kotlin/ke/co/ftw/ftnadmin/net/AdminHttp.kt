package ke.co.ftw.ftnadmin.net

import android.net.Network
import android.util.Base64
import ke.co.ftw.ftnadmin.BuildConfig
import ke.co.ftw.ftnadmin.auth.Credentials
import ke.co.ftw.ftnadmin.tunnel.HealthProbe
import java.io.ByteArrayOutputStream
import java.io.File
import java.io.IOException
import java.io.InputStream
import java.net.HttpURLConnection
import java.net.URL
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext

/** Minimal blocking HTTP client over HttpURLConnection. Callers run it on Dispatchers.IO. */
object AdminHttp {
    const val ORIGIN: String = BuildConfig.ADMIN_ORIGIN
    private const val MAX_BODY = 1 shl 20
    const val MAX_DOWNLOAD = 50L shl 20

    class Response(val code: Int, headers: Map<String, String>, val body: String) {
        private val lower = headers.mapKeys { it.key.lowercase() }
        fun header(name: String): String? = lower[name.lowercase()]
    }

    fun basicAuth(c: Credentials): String =
        "Basic " + Base64.encodeToString("${c.username}:${c.password}".toByteArray(Charsets.UTF_8), Base64.NO_WRAP)

    fun open(url: String, network: Network?, headers: Map<String, String>, timeoutMs: Int): HttpURLConnection {
        val u = URL(url)
        val conn = (network?.openConnection(u) ?: u.openConnection()) as HttpURLConnection
        conn.connectTimeout = timeoutMs
        conn.readTimeout = timeoutMs
        // Never follow redirects: the Authorization header must not travel to another host.
        conn.instanceFollowRedirects = false
        conn.useCaches = false
        conn.requestMethod = "GET"
        headers.forEach { (k, v) -> conn.setRequestProperty(k, v) }
        return conn
    }

    @Throws(IOException::class)
    fun get(url: String, network: Network?, headers: Map<String, String> = emptyMap(), timeoutMs: Int = 6_000): Response {
        val conn = open(url, network, headers, timeoutMs)
        try {
            val code = conn.responseCode
            val stream = if (code >= 400) conn.errorStream else conn.inputStream
            val body = stream?.use { readLimited(it, MAX_BODY) }.orEmpty()
            val hdrs = buildMap {
                for ((k, v) in conn.headerFields) if (k != null) put(k, v.firstOrNull().orEmpty())
            }
            return Response(code, hdrs, body)
        } finally {
            conn.disconnect()
        }
    }

    /** Streams the body into [target], failing when it exceeds [MAX_DOWNLOAD]. */
    @Throws(IOException::class)
    fun copyLimited(input: InputStream, target: File) {
        target.outputStream().use { out ->
            val buf = ByteArray(16 * 1024)
            var total = 0L
            while (true) {
                val n = input.read(buf)
                if (n < 0) break
                total += n
                if (total > MAX_DOWNLOAD) throw IOException("The export is larger than ${MAX_DOWNLOAD shr 20} MB.")
                out.write(buf, 0, n)
            }
        }
    }

    private fun readLimited(input: InputStream, limit: Int): String {
        val out = ByteArrayOutputStream()
        val buf = ByteArray(8 * 1024)
        while (out.size() < limit) {
            val n = input.read(buf, 0, minOf(buf.size, limit - out.size()))
            if (n < 0) break
            out.write(buf, 0, n)
        }
        return out.toString(Charsets.UTF_8.name())
    }

    /** The tunnel ready gate: the unauthenticated `/healthz` must answer 200 within 1.5 s. */
    val healthProbe = HealthProbe { network ->
        withContext(Dispatchers.IO) {
            try {
                val r = get("$ORIGIN/healthz", network, timeoutMs = 1_500)
                r.code == 200 && JsonParsers.parseHealth(r.body)?.ok == true
            } catch (_: IOException) {
                false
            }
        }
    }
}
