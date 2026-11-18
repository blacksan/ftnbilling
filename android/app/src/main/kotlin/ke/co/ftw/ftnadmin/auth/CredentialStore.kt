package ke.co.ftw.ftnadmin.auth

import ke.co.ftw.ftnadmin.core.crypto.SecretStore
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow

data class Credentials(val username: String, val password: String) {
    override fun toString() = "Credentials(username=$username, password=***)"
}

/**
 * The admin Basic-auth login. Saved credentials are encrypted (`user NUL password`) and cached in
 * memory so WebView callbacks never touch the Keystore on the main thread. A login the owner chose
 * not to save is kept in memory for this tunnel session only.
 */
class CredentialStore(private val secrets: SecretStore) {
    @Volatile private var saved: Credentials? = null
    @Volatile private var session: Credentials? = null
    private val _hasSaved = MutableStateFlow(false)
    val hasSaved: StateFlow<Boolean> = _hasSaved.asStateFlow()

    /** Reads the stored login into memory (blocking IO). */
    fun preload() {
        saved = secrets.get(ENTRY)?.let(::decode)
        _hasSaved.value = saved != null
    }

    /** Saved login, else this session's. */
    fun current(): Credentials? = saved ?: session

    fun savedUsername(): String? = saved?.username

    fun save(credentials: Credentials) {
        secrets.put(ENTRY, (credentials.username + "\u0000" + credentials.password).toByteArray(Charsets.UTF_8))
        saved = credentials
        _hasSaved.value = true
    }

    fun rememberForSession(credentials: Credentials) {
        session = credentials
    }

    fun clearSession() {
        session = null
    }

    fun forget() {
        secrets.delete(ENTRY)
        saved = null
        session = null
        _hasSaved.value = false
    }

    private fun decode(bytes: ByteArray): Credentials? {
        val text = bytes.toString(Charsets.UTF_8)
        val nul = text.indexOf('\u0000')
        return if (nul <= 0) null else Credentials(text.substring(0, nul), text.substring(nul + 1))
    }

    private companion object {
        const val ENTRY = "creds"
    }
}
