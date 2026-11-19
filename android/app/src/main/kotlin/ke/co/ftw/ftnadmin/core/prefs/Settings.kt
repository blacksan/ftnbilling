package ke.co.ftw.ftnadmin.core.prefs

import android.content.Context
import androidx.datastore.core.DataStore
import androidx.datastore.preferences.core.Preferences
import androidx.datastore.preferences.core.booleanPreferencesKey
import androidx.datastore.preferences.core.edit
import androidx.datastore.preferences.core.emptyPreferences
import androidx.datastore.preferences.core.intPreferencesKey
import androidx.datastore.preferences.core.longPreferencesKey
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

/** How the app picks light or dark: follow the phone, or force one. */
enum class ThemeMode(val key: String, val label: String) {
    System("system", "System"),
    Light("light", "Light"),
    Dark("dark", "Dark"),
    ;

    /** True when the UI must be dark, given what the phone is currently set to. */
    fun isDark(systemDark: Boolean): Boolean = when (this) {
        System -> systemDark
        Light -> false
        Dark -> true
    }

    companion object {
        fun fromKey(key: String?): ThemeMode = entries.firstOrNull { it.key == key } ?: System
    }
}

/** Non-secret settings. Secrets live in [ke.co.ftw.ftnadmin.core.crypto.SecretStore]. */
data class AppSettings(
    val biometricEnabled: Boolean = false,
    val lockTimeoutMin: Int = 1,
    val autoDisconnectMin: Int = 5,
    val flagSecure: Boolean = true,
    val publicBaseUrl: String = DEFAULT_PUBLIC_URL,
    val autoConnect: Boolean = false,
    val onboardingDone: Boolean = false,
    val dynamicColor: Boolean = false,
    val themeMode: ThemeMode = ThemeMode.System,
    /** Router chosen in the top-bar switcher; blank means "All routers". */
    val selectedRouterId: String = "",
    /** Release builds look for a newer app version on start (at most every 6 hours). */
    val updateCheckEnabled: Boolean = true,
    /** Epoch millis of the last update check, 0 = never. */
    val updateLastCheckAt: Long = 0L,
    /** Plain-words result of the last check, shown in Settings. */
    val updateLastResult: String = "",
    /** JSON of the update found by the last check, so the banner survives a restart; blank = none. */
    val updatePending: String = "",
)

private val Context.dataStore by preferencesDataStore(name = "ftn_settings")

class Settings(private val store: DataStore<Preferences>, scope: CoroutineScope) {
    constructor(context: Context, scope: CoroutineScope) : this(context.applicationContext.dataStore, scope)

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
            p[DYNAMIC_COLOR] = n.dynamicColor
            p[THEME_MODE] = n.themeMode.key
            p[SELECTED_ROUTER] = n.selectedRouterId
            p[UPDATE_ENABLED] = n.updateCheckEnabled
            p[UPDATE_LAST_AT] = n.updateLastCheckAt
            p[UPDATE_LAST_RESULT] = n.updateLastResult
            p[UPDATE_PENDING] = n.updatePending
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
            dynamicColor = this[DYNAMIC_COLOR] ?: d.dynamicColor,
            themeMode = ThemeMode.fromKey(this[THEME_MODE]),
            selectedRouterId = this[SELECTED_ROUTER] ?: d.selectedRouterId,
            updateCheckEnabled = this[UPDATE_ENABLED] ?: d.updateCheckEnabled,
            updateLastCheckAt = this[UPDATE_LAST_AT] ?: d.updateLastCheckAt,
            updateLastResult = this[UPDATE_LAST_RESULT] ?: d.updateLastResult,
            updatePending = this[UPDATE_PENDING] ?: d.updatePending,
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
        val DYNAMIC_COLOR = booleanPreferencesKey("dynamic_color")
        val THEME_MODE = stringPreferencesKey("theme_mode")
        val SELECTED_ROUTER = stringPreferencesKey("selected_router")
        val UPDATE_ENABLED = booleanPreferencesKey("update_check_enabled")
        val UPDATE_LAST_AT = longPreferencesKey("update_last_check_at")
        val UPDATE_LAST_RESULT = stringPreferencesKey("update_last_result")
        val UPDATE_PENDING = stringPreferencesKey("update_pending")
    }
}
