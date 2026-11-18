package ke.co.ftw.ftnadmin.core.network

import ke.co.ftw.ftnadmin.core.crypto.SecretStore

/** Keeps the session only in [SecretStore] (Keystore AES-GCM). Tokens are never written anywhere else. */
class SecretTokenStore(private val secrets: SecretStore) : TokenStore {
    override fun load(): Session? {
        val bytes = secrets.get(NAME) ?: return null
        return try {
            ApiJson.decodeFromString(Session.serializer(), bytes.decodeToString())
        } catch (_: Exception) {
            secrets.delete(NAME)
            null
        }
    }

    override fun save(session: Session) {
        secrets.put(NAME, ApiJson.encodeToString(Session.serializer(), session).encodeToByteArray())
    }

    override fun clear() = secrets.delete(NAME)

    private companion object {
        const val NAME = "api_session"
    }
}
