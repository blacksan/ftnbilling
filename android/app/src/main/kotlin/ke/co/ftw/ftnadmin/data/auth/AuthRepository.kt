package ke.co.ftw.ftnadmin.data.auth

import ke.co.ftw.ftnadmin.core.network.ApiClient
import ke.co.ftw.ftnadmin.core.network.ApiError
import ke.co.ftw.ftnadmin.core.network.AuthState
import ke.co.ftw.ftnadmin.core.network.Session
import ke.co.ftw.ftnadmin.core.network.SessionManager
import ke.co.ftw.ftnadmin.data.putIfNotBlank
import ke.co.ftw.ftnadmin.domain.DeviceSession
import ke.co.ftw.ftnadmin.domain.ItemList
import kotlinx.coroutines.flow.StateFlow
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put

/** What identifies this phone to the server, shown later in the device list. */
data class DeviceInfo(
    val installId: String,
    val name: String,
    val model: String,
    val appVersion: String,
)

/** Second-factor input from the login form. At most one is used; TOTP wins. */
data class SecondFactor(val totp: String = "", val recoveryCode: String = "", val setupCode: String = "")

class AuthRepository(
    private val api: ApiClient,
    private val sessions: SessionManager,
    private val device: () -> DeviceInfo,
) {
    val state: StateFlow<AuthState> = sessions.state

    suspend fun login(username: String, password: String, factor: SecondFactor) {
        val d = device()
        val body = buildJsonObject {
            put("username", username.trim())
            put("password", password)
            if (factor.totp.isNotBlank()) {
                put("totp", factor.totp.filter { it.isDigit() })
            } else {
                putIfNotBlank("recovery_code", factor.recoveryCode.trim())
            }
            putIfNotBlank("setup_code", factor.setupCode.trim())
            put(
                "device",
                buildJsonObject {
                    put("install_id", d.installId)
                    put("name", d.name)
                    put("model", d.model)
                    put("platform", "android")
                    put("app_version", d.appVersion)
                },
            )
        }
        val session = api.post<Session>("auth", "login", body = body)
        sessions.signIn(session)
    }

    /** Revokes this device on the server, then forgets the tokens even when the server is unreachable. */
    suspend fun logout() {
        try {
            api.send("POST", "auth", "logout")
        } catch (_: ApiError) {
            // Offline or already revoked: local sign-out below is what matters.
        } finally {
            sessions.signOut()
        }
    }

    suspend fun devices(): List<DeviceSession> = api.get<ItemList<DeviceSession>>("auth", "devices").items

    suspend fun revokeDevice(id: String) = api.send("DELETE", "auth", "devices", id)
}
