package ke.co.ftw.ftnadmin.net

import ke.co.ftw.ftnadmin.core.prefs.DEFAULT_PUBLIC_URL
import ke.co.ftw.ftnadmin.core.prefs.Settings
import java.io.IOException
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext

/** The public `/healthz`, reachable without the tunnel. */
class PublicHealth(private val settings: Settings) {
    suspend fun fetch(): Fetch<ServerHealth> = withContext(Dispatchers.IO) {
        val base = (settings.state.value?.publicBaseUrl ?: DEFAULT_PUBLIC_URL).trimEnd('/')
        try {
            val r = AdminHttp.get("$base/healthz", network = null)
            val parsed = if (r.code == 200) JsonParsers.parseHealth(r.body) else null
            if (parsed != null) Fetch.Ok(parsed) else Fetch.Failed("Unexpected answer (HTTP ${r.code}).")
        } catch (e: IOException) {
            Fetch.Failed(e.message ?: "Network error")
        }
    }
}
