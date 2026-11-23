package ke.co.ftw.ftnadmin

import android.app.Application
import android.os.Build
import android.os.SystemClock
import ke.co.ftw.ftnadmin.auth.AppLock
import ke.co.ftw.ftnadmin.core.crypto.KeystoreAead
import ke.co.ftw.ftnadmin.core.crypto.SecretStore
import ke.co.ftw.ftnadmin.core.network.ApiClient
import ke.co.ftw.ftnadmin.core.network.SecretKnownTokenStore
import ke.co.ftw.ftnadmin.core.network.SecretTokenStore
import ke.co.ftw.ftnadmin.core.network.SessionManager
import ke.co.ftw.ftnadmin.core.prefs.Settings
import ke.co.ftw.ftnadmin.data.accounts.AccountsRepository
import ke.co.ftw.ftnadmin.data.alerts.AlertsRepository
import ke.co.ftw.ftnadmin.data.apartments.ApartmentsRepository
import ke.co.ftw.ftnadmin.data.auth.AuthRepository
import ke.co.ftw.ftnadmin.data.auth.DeviceInfo
import ke.co.ftw.ftnadmin.data.dashboard.DashboardRepository
import ke.co.ftw.ftnadmin.data.onu.OnuRepository
import ke.co.ftw.ftnadmin.data.payments.PaymentsRepository
import ke.co.ftw.ftnadmin.data.routers.RouterContext
import ke.co.ftw.ftnadmin.data.routers.RouterLiveRepository
import ke.co.ftw.ftnadmin.data.equipment.EquipmentRepository
import ke.co.ftw.ftnadmin.data.sites.NewDevicesRepository
import ke.co.ftw.ftnadmin.data.sites.SiteSetupRepository
import ke.co.ftw.ftnadmin.data.sites.SitesRepository
import ke.co.ftw.ftnadmin.data.update.AndroidApkInspector
import ke.co.ftw.ftnadmin.data.update.AndroidApkInstaller
import ke.co.ftw.ftnadmin.data.update.UpdateManager
import ke.co.ftw.ftnadmin.data.update.UpdateRepository
import java.io.File
import java.util.UUID
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

/** Hand-written dependency graph (single module, no Hilt); one instance for the process, owned by [FtnApp]. */
class AppContainer(app: Application) {
    val appScope = CoroutineScope(SupervisorJob() + Dispatchers.Default)

    private val secrets = SecretStore(app, KeystoreAead())

    val settings = Settings(app, appScope)
    val appLock = AppLock(settings, appScope)
    val windowSecurity = WindowSecurity()

    val sessions = SessionManager(SecretTokenStore(secrets))
    val api: ApiClient = ApiClient.create(BuildConfig.API_BASE, sessions)

    private val knownToken = SecretKnownTokenStore(secrets)
    val auth = AuthRepository(api, sessions, ::deviceInfo, knownToken)
    val dashboard = DashboardRepository(api)
    val accounts = AccountsRepository(api)
    val payments = PaymentsRepository(api)
    val onu = OnuRepository(api)
    val equipment = EquipmentRepository(api)
    val sites = SitesRepository(api)
    val siteSetup = SiteSetupRepository(api)
    val newDevices = NewDevicesRepository(api)
    val apartments = ApartmentsRepository(api)
    val alerts = AlertsRepository(api)
    val routerLive = RouterLiveRepository(api)
    val routerContext = RouterContext(sites, settings, appScope)

    /** In-app update check. Its HTTP client has no auth interceptors: the admin token never goes to GitHub. */
    val updates = UpdateManager(
        source = UpdateRepository(
            http = ApiClient.plainHttpClient(),
            currentVersionCode = BuildConfig.VERSION_CODE.toLong(),
            currentVersionName = BuildConfig.VERSION_NAME,
            packageName = app.packageName,
            updatesDir = File(app.cacheDir, "updates"),
            inspector = AndroidApkInspector(app),
        ),
        settings = settings,
        installer = AndroidApkInstaller(app),
        scope = appScope,
        enabled = BuildConfig.UPDATES_ENABLED,
        currentVersionCode = BuildConfig.VERSION_CODE.toLong(),
    )

    init {
        appScope.launch(Dispatchers.IO) { sessions.restore() }
    }

    fun onForeground() = appLock.onForeground(SystemClock.elapsedRealtime())

    fun onBackground() = appLock.onBackground(SystemClock.elapsedRealtime())

    /** Revokes this device on the server and forgets the tokens. */
    fun logout() {
        appScope.launch { auth.logout() }
    }

    /** Removes the session and every setting from this device. */
    suspend fun wipe() {
        withContext(Dispatchers.IO) {
            sessions.signOut()
            knownToken.clear()
            secrets.wipeAll()
            settings.reset()
        }
        appLock.reset()
    }

    private fun deviceInfo(): DeviceInfo {
        val id = secrets.get(INSTALL_ID)?.decodeToString() ?: UUID.randomUUID().toString().also {
            secrets.put(INSTALL_ID, it.encodeToByteArray())
        }
        val model = Build.MODEL.orEmpty()
        return DeviceInfo(
            installId = id,
            name = "${Build.MANUFACTURER.orEmpty().replaceFirstChar { it.uppercase() }} $model".trim(),
            model = model,
            appVersion = BuildConfig.VERSION_NAME,
        )
    }

    private companion object {
        const val INSTALL_ID = "install_id"
    }
}

/** Counts screens that must hide their content from screenshots regardless of the user setting. */
class WindowSecurity {
    private val _forced = MutableStateFlow(0)
    val forced: StateFlow<Int> = _forced.asStateFlow()

    fun acquire() = _forced.update { it + 1 }

    fun release() = _forced.update { it - 1 }
}
