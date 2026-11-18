package ke.co.ftw.ftnadmin

import android.app.Application
import android.os.SystemClock
import android.webkit.CookieManager
import com.wireguard.android.backend.Backend
import com.wireguard.android.backend.GoBackend
import ke.co.ftw.ftnadmin.auth.AppLock
import ke.co.ftw.ftnadmin.auth.CredentialStore
import ke.co.ftw.ftnadmin.auth.LockState
import ke.co.ftw.ftnadmin.core.crypto.KeystoreAead
import ke.co.ftw.ftnadmin.core.crypto.SecretStore
import ke.co.ftw.ftnadmin.core.prefs.Settings
import ke.co.ftw.ftnadmin.net.AdminApi
import ke.co.ftw.ftnadmin.net.AdminHttp
import ke.co.ftw.ftnadmin.net.PublicHealth
import ke.co.ftw.ftnadmin.tunnel.TunnelManager
import ke.co.ftw.ftnadmin.tunnel.TunnelRepository
import ke.co.ftw.ftnadmin.tunnel.TunnelState
import ke.co.ftw.ftnadmin.web.AdminWebController
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

/** Hand-written dependency graph; one instance for the process, owned by [FtnApp]. */
class AppContainer(private val app: Application) {
    val appScope = CoroutineScope(SupervisorJob() + Dispatchers.Default)

    private val backend: Backend = GoBackend(app)
    private val secrets = SecretStore(app, KeystoreAead())

    val settings = Settings(app, appScope)
    val tunnelRepository = TunnelRepository(secrets)
    val credentials = CredentialStore(secrets)
    val tunnel = TunnelManager(app, backend, tunnelRepository, settings, AdminHttp.healthProbe, appScope)
    val appLock = AppLock(settings, appScope)
    val adminApi = AdminApi(tunnel, credentials)
    val publicHealth = PublicHealth(settings)
    val windowSecurity = WindowSecurity()

    /** True after a wipe until the onboarding screen has shown the reminder to revoke the device. */
    private val _wiped = MutableStateFlow(false)
    val wiped: StateFlow<Boolean> = _wiped.asStateFlow()

    private val foreground = MutableStateFlow(false)

    init {
        appScope.launch(Dispatchers.IO) {
            tunnelRepository.load()
            credentials.preload()
        }
        appScope.launch {
            // A login that was not saved only lives as long as the tunnel session.
            tunnel.state.collect { if (it !is TunnelState.Up) credentials.clearSession() }
        }
        appScope.launch {
            // Auto-connect when the app comes to the foreground unlocked; not after a manual disconnect.
            combine(foreground, appLock.state) { fg, lock -> fg && lock == LockState.Unlocked }
                .distinctUntilChanged()
                .collect { ready ->
                    val s = settings.state.value
                    if (ready && s != null && s.autoConnect && s.onboardingDone && tunnel.state.value is TunnelState.Down) {
                        tunnel.connect()
                    }
                }
        }
    }

    fun onForeground() {
        foreground.value = true
        tunnel.onAppForeground()
        appLock.onForeground(SystemClock.elapsedRealtime())
    }

    fun onBackground() {
        foreground.value = false
        appLock.onBackground(SystemClock.elapsedRealtime())
        tunnel.onAppBackground()
    }

    /** The server's logout always answers 401, so logging out natively means: forget the session, drop the tunnel, lock. */
    fun logout() {
        CookieManager.getInstance().removeAllCookies(null)
        appLock.lock()
        appScope.launch { tunnel.disconnect() }
    }

    /** Removes every trace of the tunnel, login and settings from this device. */
    suspend fun wipe() {
        tunnel.disconnect()
        withContext(Dispatchers.IO) {
            tunnelRepository.clear()
            credentials.forget()
            secrets.wipeAll()
            settings.reset()
        }
        AdminWebController.clearAllWebData(app)
        appLock.reset()
        _wiped.value = true
    }

    fun acknowledgeWipe() {
        _wiped.value = false
    }
}

/** Counts screens that must hide their content from screenshots regardless of the user setting. */
class WindowSecurity {
    private val _forced = MutableStateFlow(0)
    val forced: StateFlow<Int> = _forced.asStateFlow()

    fun acquire() = _forced.update { it + 1 }

    fun release() = _forced.update { it - 1 }
}
