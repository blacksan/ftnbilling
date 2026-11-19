package ke.co.ftw.ftnadmin.core.network

import java.io.IOException
import java.util.concurrent.TimeUnit
import kotlin.coroutines.resumeWithException
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineDispatcher
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.suspendCancellableCoroutine
import kotlinx.coroutines.withContext
import kotlinx.serialization.KSerializer
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.contentOrNull
import kotlinx.serialization.serializer
import okhttp3.Call
import okhttp3.Callback
import okhttp3.HttpUrl
import okhttp3.HttpUrl.Companion.toHttpUrl
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import okhttp3.Response

/**
 * Thin JSON client for `<base>/api/v1`. Every failure surfaces as [ApiError].
 *
 * Paths are given as segments so ids are percent-encoded: `api.get<Account>("accounts", id)`.
 */
class ApiClient(
    baseUrl: HttpUrl,
    private val http: OkHttpClient,
    val json: Json = ApiJson,
    /** Where response bodies are read. Tests pass their own dispatcher so no work hops to a real thread. */
    private val io: CoroutineDispatcher = Dispatchers.IO,
) {
    private val root: HttpUrl = baseUrl.newBuilder().addPathSegments("api/v1").build()

    fun url(vararg segments: String, query: Map<String, String?> = emptyMap()): HttpUrl =
        root.newBuilder().apply {
            segments.forEach { addPathSegment(it) }
            query.forEach { (k, v) -> if (!v.isNullOrBlank()) addQueryParameter(k, v) }
        }.build()

    suspend inline fun <reified T> get(vararg segments: String, query: Map<String, String?> = emptyMap()): T =
        request("GET", segments.toList(), query, null, serializer<T>())

    suspend inline fun <reified T> post(vararg segments: String, body: JsonObject? = null): T =
        request("POST", segments.toList(), emptyMap(), body, serializer<T>())

    suspend inline fun <reified T> put(vararg segments: String, body: JsonObject? = null): T =
        request("PUT", segments.toList(), emptyMap(), body, serializer<T>())

    suspend inline fun <reified T> patch(vararg segments: String, body: JsonObject? = null): T =
        request("PATCH", segments.toList(), emptyMap(), body, serializer<T>())

    suspend inline fun <reified T> delete(vararg segments: String): T =
        request("DELETE", segments.toList(), emptyMap(), null, serializer<T>())

    /** For endpoints that answer 204 or whose body is not needed. */
    suspend fun send(method: String, vararg segments: String, body: JsonObject? = null) {
        requestRaw(method, segments.toList(), emptyMap(), body)
    }

    @PublishedApi
    internal suspend fun <T> request(
        method: String,
        segments: List<String>,
        query: Map<String, String?>,
        body: JsonObject?,
        ser: KSerializer<T>,
    ): T {
        val text = requestRaw(method, segments, query, body)
        return try {
            json.decodeFromString(ser, text)
        } catch (e: CancellationException) {
            throw e
        } catch (e: Exception) {
            throw ApiError(200, ErrorCode.UNKNOWN, "The server sent a response the app cannot read.", cause = e)
        }
    }

    @PublishedApi
    internal suspend fun requestRaw(
        method: String,
        segments: List<String>,
        query: Map<String, String?>,
        body: JsonObject?,
    ): String {
        val url = url(*segments.toTypedArray(), query = query)
        val rb = when {
            body != null -> body.toString().toRequestBody(JSON)
            method == "GET" -> null
            else -> "".toRequestBody(null)
        }
        val req = Request.Builder().url(url).method(method, rb).header("Accept", "application/json").build()
        val response = http.newCall(req).await()
        return withContext(io) {
            response.use { r ->
                val text = try {
                    r.body?.string().orEmpty()
                } catch (e: IOException) {
                    throw ApiError.network(e)
                }
                if (!r.isSuccessful) throw parseError(r, text)
                text.ifBlank { "null" }.let { if (it == "null") "{}" else it }
            }
        }
    }

    private fun parseError(r: Response, text: String): ApiError {
        val env = try {
            (json.parseToJsonElement(text) as? JsonObject)?.get("error") as? JsonObject
        } catch (_: Exception) {
            null
        }
        val details = env?.get("details") as? JsonObject
        val code = (env?.get("code") as? JsonPrimitive)?.contentOrNull
        val message = (env?.get("message") as? JsonPrimitive)?.contentOrNull
        val retry = ApiError.retryAfter(r.header("Retry-After"), details)
        return ApiError(
            status = r.code,
            code = code ?: if (r.code == 401) ErrorCode.TOKEN_EXPIRED else ErrorCode.UNKNOWN,
            message = message ?: "The server answered with an error (HTTP ${r.code}).",
            details = details,
            retryAfterS = retry,
        )
    }

    @OptIn(ExperimentalCoroutinesApi::class) // resume(value, onCancellation)
    private suspend fun Call.await(): Response = suspendCancellableCoroutine { cont ->
        cont.invokeOnCancellation { cancel() }
        enqueue(
            object : Callback {
                override fun onFailure(call: Call, e: IOException) {
                    if (cont.isActive) cont.resumeWithException(ApiError.network(e))
                }

                override fun onResponse(call: Call, response: Response) {
                    // If the caller was cancelled while the response was arriving, nobody will read it: close it
                    // so the connection goes back to the pool instead of leaking.
                    if (!cont.isActive) {
                        response.close()
                        return
                    }
                    cont.resume(response) { _ -> response.close() }
                }
            },
        )
    }

    companion object {
        private val JSON = "application/json; charset=utf-8".toMediaType()

        /** An OkHttp client with the app's timeouts and no auth: also used for the GitHub update check. */
        fun plainHttpClient(): OkHttpClient = OkHttpClient.Builder()
            .connectTimeout(15, TimeUnit.SECONDS)
            .readTimeout(30, TimeUnit.SECONDS)
            .writeTimeout(30, TimeUnit.SECONDS)
            .build()

        /**
         * Builds the production client. [baseUrl] is the server root, e.g. `https://isp.ftw.co.ke`.
         * The refresh client is separate so refreshing never recurses into the authenticator.
         */
        fun create(
            baseUrl: String,
            sessions: SessionManager,
            json: Json = ApiJson,
            io: CoroutineDispatcher = Dispatchers.IO,
        ): ApiClient {
            val base = baseUrl.toHttpUrl()
            val plain = plainHttpClient()
            val refreshUrl = base.newBuilder().addPathSegments("api/v1/auth/refresh").build()
            val http = plain.newBuilder()
                .addInterceptor(BearerInterceptor(sessions))
                .addInterceptor(IdempotencyInterceptor())
                .authenticator(TokenAuthenticator(sessions, plain, refreshUrl, json))
                .build()
            return ApiClient(base, http, json, io)
        }
    }
}
