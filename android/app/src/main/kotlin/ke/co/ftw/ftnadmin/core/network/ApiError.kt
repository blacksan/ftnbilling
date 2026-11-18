package ke.co.ftw.ftnadmin.core.network

import java.io.IOException
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.booleanOrNull
import kotlinx.serialization.json.contentOrNull
import kotlinx.serialization.json.intOrNull

/** Error codes from the API contract (`docs/api-v1.md`, "Errors"). */
object ErrorCode {
    const val BAD_REQUEST = "bad_request"
    const val VALIDATION_FAILED = "validation_failed"
    const val INVALID_CREDENTIALS = "invalid_credentials"
    const val TOTP_INVALID = "totp_invalid"
    const val TOKEN_EXPIRED = "token_expired"
    const val TOKEN_REVOKED = "token_revoked"
    const val TOTP_ENROL_REQUIRED = "totp_enrol_required"
    const val FORBIDDEN_ROLE = "forbidden_role"
    const val NOT_FOUND = "not_found"
    const val MAC_IN_USE = "mac_in_use"
    const val FLAT_IN_USE = "flat_in_use"
    const val DUPLICATE_PAYMENT = "duplicate_payment"
    const val LOWER_PLAN_ACTIVE = "lower_plan_active"
    const val BAD_STATE = "bad_state"
    const val RATE_LIMITED = "rate_limited"
    const val DB_UNAVAILABLE = "db_unavailable"

    /** Not from the server: no connection, timeout, TLS failure or unreadable response. */
    const val NETWORK = "network"
    const val UNKNOWN = "unknown"
}

/** The account that already owns a MAC (details of a 409 `mac_in_use`). */
data class MacOwner(
    val mac: String,
    val accountId: String,
    val code: String,
    val flat: String,
    val siteId: String,
    val siteName: String,
)

/**
 * Any failed API call: an error envelope from the server (status > 0) or a transport problem
 * (status 0, code [ErrorCode.NETWORK]).
 */
class ApiError(
    val status: Int,
    val code: String,
    override val message: String,
    val details: JsonObject? = null,
    val retryAfterS: Int? = null,
    cause: Throwable? = null,
) : IOException(message, cause) {
    val isNetwork: Boolean get() = status == 0

    /** Per-field validation messages from `details.fields`. */
    val fieldErrors: Map<String, String>
        get() = (details?.get("fields") as? JsonObject)
            ?.mapNotNull { (k, v) -> (v as? JsonPrimitive)?.contentOrNull?.let { k to it } }
            ?.toMap()
            .orEmpty()

    /** Login: after 10 wrong authenticator codes only a recovery code is accepted (`details.recovery_code_required`). */
    val recoveryCodeRequired: Boolean
        get() = (details?.get("recovery_code_required") as? JsonPrimitive)?.booleanOrNull == true

    /** For `mac_in_use`: who owns the MAC. Null when the details are missing or malformed. */
    val macOwner: MacOwner?
        get() {
            if (code != ErrorCode.MAC_IN_USE) return null
            val d = details ?: return null
            val acc = d["account"] as? JsonObject ?: return null
            fun s(o: JsonObject, k: String) = (o[k] as? JsonPrimitive)?.contentOrNull.orEmpty()
            return MacOwner(
                mac = s(d, "mac"),
                accountId = s(acc, "id"),
                code = s(acc, "code"),
                flat = s(acc, "flat"),
                siteId = s(acc, "site_id"),
                siteName = s(acc, "site_name"),
            )
        }

    /** True when the session is gone and the user must sign in again. */
    val needsLogin: Boolean get() = status == 401 && code != ErrorCode.INVALID_CREDENTIALS && code != ErrorCode.TOTP_INVALID

    companion object {
        fun network(cause: Throwable) = ApiError(
            0, ErrorCode.NETWORK, "Cannot reach the server. Check your connection and try again.", cause = cause,
        )

        /** Retry-After header or `details.retry_after_s`. */
        fun retryAfter(header: String?, details: JsonObject?): Int? =
            (details?.get("retry_after_s") as? JsonPrimitive)?.intOrNull ?: header?.trim()?.toIntOrNull()
    }
}
