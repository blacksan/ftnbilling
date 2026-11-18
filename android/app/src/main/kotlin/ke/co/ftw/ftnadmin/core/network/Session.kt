package ke.co.ftw.ftnadmin.core.network

import java.util.concurrent.atomic.AtomicReference
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable

@Serializable
data class UserInfo(
    val id: String = "",
    val username: String = "",
    val role: String = "viewer",
) {
    val canWrite: Boolean get() = role == "owner" || role == "operator"
    val isOwner: Boolean get() = role == "owner"
}

/** Token pair as stored on the device and as returned by `/auth/login` and `/auth/refresh`. */
@Serializable
data class Session(
    @SerialName("access_token") val accessToken: String,
    @SerialName("access_expires_at") val accessExpiresAt: String = "",
    @SerialName("refresh_token") val refreshToken: String,
    @SerialName("refresh_expires_at") val refreshExpiresAt: String = "",
    @SerialName("device_id") val deviceId: String = "",
    val user: UserInfo = UserInfo(),
)

/** `/auth/refresh` answers the same fields without `user` when the user did not change. */
@Serializable
data class RefreshResponse(
    @SerialName("access_token") val accessToken: String,
    @SerialName("access_expires_at") val accessExpiresAt: String = "",
    @SerialName("refresh_token") val refreshToken: String,
    @SerialName("refresh_expires_at") val refreshExpiresAt: String = "",
    @SerialName("device_id") val deviceId: String? = null,
    val user: UserInfo? = null,
)

/** Where tokens live between runs. The production implementation encrypts into SecretStore. */
interface TokenStore {
    fun load(): Session?

    fun save(session: Session)

    fun clear()
}

sealed interface AuthState {
    data object Loading : AuthState

    /** [reason] says why the user was signed out when it was not their own doing. */
    data class SignedOut(val reason: String? = null) : AuthState

    data class SignedIn(val user: UserInfo) : AuthState
}

/** Single owner of the current tokens. Thread-safe: OkHttp reads it from its own threads. */
class SessionManager(private val store: TokenStore) {
    private val ref = AtomicReference<Session?>(null)
    private val _state = MutableStateFlow<AuthState>(AuthState.Loading)
    val state: StateFlow<AuthState> = _state.asStateFlow()

    val current: Session? get() = ref.get()

    /** Reads the stored session; call once at start-up off the main thread. */
    fun restore() {
        val s = runCatching { store.load() }.getOrNull()
        ref.set(s)
        _state.value = if (s == null) AuthState.SignedOut() else AuthState.SignedIn(s.user)
    }

    fun signIn(session: Session) {
        store.save(session)
        ref.set(session)
        _state.value = AuthState.SignedIn(session.user)
    }

    /** Replaces tokens after a refresh; keeps the user and device id when the response omits them. */
    fun rotate(r: RefreshResponse): Session {
        val old = ref.get()
        val next = Session(
            accessToken = r.accessToken,
            accessExpiresAt = r.accessExpiresAt,
            refreshToken = r.refreshToken,
            refreshExpiresAt = r.refreshExpiresAt,
            deviceId = r.deviceId ?: old?.deviceId.orEmpty(),
            user = r.user ?: old?.user ?: UserInfo(),
        )
        store.save(next)
        ref.set(next)
        return next
    }

    /** Forgets the tokens. [reason] is shown on the login screen. */
    fun signOut(reason: String? = null) {
        ref.set(null)
        runCatching { store.clear() }
        _state.value = AuthState.SignedOut(reason)
    }
}
