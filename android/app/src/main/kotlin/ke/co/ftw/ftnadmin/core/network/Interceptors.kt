package ke.co.ftw.ftnadmin.core.network

import java.util.UUID
import okhttp3.Interceptor
import okhttp3.Response

internal fun isAuthPath(segments: List<String>): Boolean =
    segments.size >= 4 && segments[0] == "api" && segments[2] == "auth" &&
        (segments[3] == "login" || segments[3] == "refresh")

/** Adds `Authorization: Bearer` to every call except login and refresh. */
class BearerInterceptor(private val sessions: SessionManager) : Interceptor {
    override fun intercept(chain: Interceptor.Chain): Response {
        val req = chain.request()
        if (isAuthPath(req.url.pathSegments) || req.header("Authorization") != null) return chain.proceed(req)
        val token = sessions.current?.accessToken ?: return chain.proceed(req)
        return chain.proceed(req.newBuilder().header("Authorization", "Bearer $token").build())
    }
}

/** The join line is a one-time secret (join-command and the reset answer): the server must never replay a stored copy. */
internal fun isJoinCommandPath(segments: List<String>): Boolean =
    segments.lastOrNull() == "join-command" || (segments.takeLast(2) == listOf("actions", "reset"))

/**
 * Adds an `Idempotency-Key` to mutating calls so the server can replay the stored response if a
 * request is repeated (after a refresh retry or a flaky connection). The key is created once per
 * request and copied by OkHttp's retry paths because they reuse the request headers.
 */
class IdempotencyInterceptor(private val newKey: () -> String = { UUID.randomUUID().toString() }) : Interceptor {
    override fun intercept(chain: Interceptor.Chain): Response {
        val req = chain.request()
        val mutating = req.method == "POST" || req.method == "PUT" || req.method == "PATCH" || req.method == "DELETE"
        if (!mutating || req.header("Idempotency-Key") != null || isAuthPath(req.url.pathSegments) ||
            isJoinCommandPath(req.url.pathSegments)
        ) {
            return chain.proceed(req)
        }
        return chain.proceed(req.newBuilder().header("Idempotency-Key", newKey()).build())
    }
}
