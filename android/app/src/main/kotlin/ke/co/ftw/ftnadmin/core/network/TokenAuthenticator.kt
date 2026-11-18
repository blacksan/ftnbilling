package ke.co.ftw.ftnadmin.core.network

import java.io.IOException
import okhttp3.Authenticator
import okhttp3.HttpUrl
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import okhttp3.Response
import okhttp3.Route
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put

/**
 * On a 401 swaps the refresh token for a new pair and retries once.
 *
 * Single flight: refreshing is serialised under [lock]. A call that finds the stored access token
 * already differs from the one it sent simply retries with the new one, so N parallel 401s cause
 * exactly one `/auth/refresh`. Refresh tokens rotate on every use, so a second parallel refresh
 * would look like token reuse to the server and revoke the device.
 *
 * [refreshClient] must NOT contain this authenticator or the bearer interceptor.
 */
class TokenAuthenticator(
    private val sessions: SessionManager,
    private val refreshClient: OkHttpClient,
    private val refreshUrl: HttpUrl,
    private val json: Json = ApiJson,
) : Authenticator {
    private val lock = Any()

    override fun authenticate(route: Route?, response: Response): Request? {
        val sent = response.request.header("Authorization")?.removePrefix("Bearer ") ?: return null
        if (isAuthPath(response.request.url.pathSegments)) return null
        if (priorCount(response) >= 2) return null

        val code = errorCode(response)
        synchronized(lock) {
            val cur = sessions.current ?: return null
            if (cur.accessToken != sent) return retry(response.request, cur.accessToken)
            if (code == ErrorCode.TOKEN_REVOKED) {
                sessions.signOut("This device was signed out by the server. Sign in again.")
                return null
            }
            return when (val r = refresh(cur)) {
                is Refresh.Ok -> retry(response.request, sessions.rotate(r.value).accessToken)
                Refresh.Rejected -> {
                    sessions.signOut("Your session ended. Sign in again.")
                    null
                }
                Refresh.Unavailable -> null
            }
        }
    }

    private sealed interface Refresh {
        data class Ok(val value: RefreshResponse) : Refresh

        /** The server said the refresh token is expired, revoked or reused. */
        data object Rejected : Refresh

        /** Network failure, rate limit or server error: keep the tokens and let the call fail. */
        data object Unavailable : Refresh
    }

    private fun refresh(cur: Session): Refresh {
        val body: JsonObject = buildJsonObject { put("refresh_token", cur.refreshToken) }
        val req = Request.Builder().url(refreshUrl)
            .post(body.toString().toRequestBody(JSON))
            .build()
        return try {
            refreshClient.newCall(req).execute().use { r ->
                val text = r.body?.string().orEmpty()
                when {
                    r.isSuccessful -> try {
                        Refresh.Ok(json.decodeFromString(RefreshResponse.serializer(), text))
                    } catch (_: Exception) {
                        Refresh.Unavailable
                    }
                    r.code == 401 -> Refresh.Rejected
                    else -> Refresh.Unavailable
                }
            }
        } catch (_: IOException) {
            Refresh.Unavailable
        }
    }

    private fun errorCode(response: Response): String? = try {
        val text = response.peekBody(PEEK_LIMIT).string()
        ((json.parseToJsonElement(text) as? JsonObject)?.get("error") as? JsonObject)
            ?.get("code")?.let { (it as? kotlinx.serialization.json.JsonPrimitive)?.content }
    } catch (_: Exception) {
        null
    }

    private fun retry(request: Request, token: String): Request =
        request.newBuilder().header("Authorization", "Bearer $token").build()

    private fun priorCount(response: Response): Int {
        var n = 1
        var r = response.priorResponse
        while (r != null) {
            n++
            r = r.priorResponse
        }
        return n
    }

    private companion object {
        val JSON = "application/json; charset=utf-8".toMediaType()
        const val PEEK_LIMIT = 16L * 1024
    }
}
