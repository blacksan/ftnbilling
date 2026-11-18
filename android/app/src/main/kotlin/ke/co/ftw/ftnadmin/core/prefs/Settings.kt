package ke.co.ftw.ftnadmin.core.prefs

import android.content.Context
import androidx.datastore.preferences.core.Preferences
import androidx.datastore.preferences.core.booleanPreferencesKey
import androidx.datastore.preferences.core.edit
import androidx.datastore.preferences.core.emptyPreferences
import androidx.datastore.preferences.core.intPreferencesKey
import androidx.datastore.preferences.core.stringPreferencesKey
import androidx.datastore.preferences.preferencesDataStore
import java.io.IOException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.SharingStarted
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.catch
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.flow.stateIn

const val DEFAULT_PUBLIC_URL = "https://isp.ftw.co.ke"

/** Non-secret settings. Secrets live in [ke.co.ftw.ftnadmin.core.crypto.SecretStore]. */
data class AppSettings(
    val biometricEnabled: Boolean = false,
    val lockTimeoutMin: Int = 1,
    val autoDisconnectMin: Int = 5,
    val flagSecure: Boolean = true,
    val publicBaseUrl: String = DEFAULT_PUBLIC_URL,
    val autoConnect: Boolean = false,
    val onboardingDone: Boolean = false,
)

private val Context.dataStore by preferencesDataStore(name = "ftn_settings")

class Settings(context: Context, scope: CoroutineScope) {
    private val store = context.applicationContext.dataStore

    val flow: Flow<AppSettings> = store.data
        .catch { if (it is IOException) emit(emptyPreferences()) else throw it }
        .map { it.toSettings() }

    /** Null until the first read completes. */
    val state: StateFlow<AppSettings?> = flow.stateIn(scope, SharingStarted.Eagerly, null)

    suspend fun update(transform: (AppSettings) -> AppSettings) {
        store.edit { p ->
            val n = transform(p.toSettings())
            p[BIOMETRIC] = n.biometricEnabled
            p[LOCK_TIMEOUT] = n.lockTimeoutMin
            p[AUTO_DISCONNECT] = n.autoDisconnectMin
            p[FLAG_SECURE] = n.flagSecure
            p[PUBLIC_URL] = n.publicBaseUrl
            p[AUTO_CONNECT] = n.autoConnect
            p[ONBOARDING_DONE] = n.onboardingDone
        }
    }

    suspend fun reset() {
        store.edit { it.clear() }
    }

    private fun Preferences.toSettings(): AppSettings {
        val d = AppSettings()
        return AppSettings(
            biometricEnabled = this[BIOMETRIC] ?: d.biometricEnabled,
            lockTimeoutMin = this[LOCK_TIMEOUT] ?: d.lockTimeoutMin,
            autoDisconnectMin = this[AUTO_DISCONNECT] ?: d.autoDisconnectMin,
            flagSecure = this[FLAG_SECURE] ?: d.flagSecure,
            publicBaseUrl = this[PUBLIC_URL] ?: d.publicBaseUrl,
            autoConnect = this[AUTO_CONNECT] ?: d.autoConnect,
            onboardingDone = this[ONBOARDING_DONE] ?: d.onboardingDone,
        )
    }

    private companion object {
        val BIOMETRIC = booleanPreferencesKey("biometric_enabled")
        val LOCK_TIMEOUT = intPreferencesKey("lock_timeout_min")
        val AUTO_DISCONNECT = intPreferencesKey("auto_disconnect_min")
        val FLAG_SECURE = booleanPreferencesKey("flag_secure")
        val PUBLIC_URL = stringPreferencesKey("public_base_url")
        val AUTO_CONNECT = booleanPreferencesKey("auto_connect")
        val ONBOARDING_DONE = booleanPreferencesKey("onboarding_done")
    }
}
