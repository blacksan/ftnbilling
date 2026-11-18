package ke.co.ftw.ftnadmin.tunnel

import ke.co.ftw.ftnadmin.core.crypto.SecretStore
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow

/**
 * Holds the encrypted tunnel config. The private key is only ever returned by [configText], which
 * exists solely for [TunnelManager]; everything else gets a [TunnelSummary].
 */
class TunnelRepository(private val secrets: SecretStore) {
    private val _summary = MutableStateFlow<TunnelSummary?>(null)
    val summary: StateFlow<TunnelSummary?> = _summary.asStateFlow()

    private val _loaded = MutableStateFlow(false)
    val loaded: StateFlow<Boolean> = _loaded.asStateFlow()

    /** Reads the stored tunnel (blocking Keystore and file IO: call off the main thread). */
    fun load() {
        val text = secrets.get(ENTRY)?.toString(Charsets.UTF_8)
        _summary.value = text?.let {
            try {
                TunnelConfigParser.parse(it).summary
            } catch (_: InvalidTunnelConfigException) {
                secrets.delete(ENTRY)
                null
            }
        }
        _loaded.value = true
    }

    /** Validates and stores a config, replacing any previous one. */
    @Throws(InvalidTunnelConfigException::class)
    fun import(text: String): TunnelSummary {
        val parsed = TunnelConfigParser.parse(text)
        secrets.put(ENTRY, parsed.sanitizedConfig.toByteArray(Charsets.UTF_8))
        _summary.value = parsed.summary
        return parsed.summary
    }

    fun configText(): String? = secrets.get(ENTRY)?.toString(Charsets.UTF_8)

    fun clear() {
        secrets.delete(ENTRY)
        _summary.value = null
    }

    private companion object {
        const val ENTRY = "tunnel"
    }
}
