package ke.co.ftw.ftnadmin.data.update

import ke.co.ftw.ftnadmin.core.prefs.Settings
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import kotlinx.serialization.SerializationException

data class DownloadProgress(val done: Long, val total: Long)

data class UpdateState(
    /** False in debug builds: the update check is switched off there. */
    val enabled: Boolean = false,
    val checking: Boolean = false,
    val available: AvailableUpdate? = null,
    /** Non-null while the APK is downloading or being verified. */
    val download: DownloadProgress? = null,
    /** The phone has not allowed this app to install apps yet. */
    val permissionNeeded: Boolean = false,
    /** Last problem, in words, from a check or a download. */
    val error: String? = null,
) {
    val busy: Boolean get() = checking || download != null
}

/**
 * Process-wide update state (the role a ViewModel plays elsewhere): Home and Settings show the same
 * state, and a download keeps going when the user leaves a screen. [source] does the network work.
 */
class UpdateManager(
    private val source: UpdateSource,
    private val settings: Settings,
    private val installer: ApkInstaller,
    private val scope: CoroutineScope,
    private val enabled: Boolean,
    private val currentVersionCode: Long,
    private val clock: () -> Long = System::currentTimeMillis,
) {
    private val _state = MutableStateFlow(UpdateState(enabled = enabled))
    val state: StateFlow<UpdateState> = _state.asStateFlow()

    /** Called once when the app process starts. */
    fun onAppStart() {
        if (!enabled) return
        scope.launch {
            source.cleanup()
            restorePending()
            autoCheck()
        }
    }

    fun checkNow() {
        scope.launch { check() }
    }

    /** Downloads and installs the available update; asks for the install permission first if needed. */
    fun startUpdate() {
        scope.launch { install() }
    }

    /** The user came back from the permission screen: carry on if it was granted. */
    fun onResumed() {
        if (_state.value.permissionNeeded && installer.canInstall()) startUpdate()
    }

    fun openInstallPermissionSettings() = installer.openPermissionSettings()

    /** Checks on start, but at most once every [CHECK_INTERVAL_MS] and only when the setting is on. */
    suspend fun autoCheck() {
        if (!enabled) return
        val s = settings.flow.first()
        if (!s.updateCheckEnabled) return
        val elapsed = clock() - s.updateLastCheckAt
        if (s.updateLastCheckAt > 0 && elapsed in 0 until CHECK_INTERVAL_MS) return
        check()
    }

    /** One check, regardless of the schedule. Records the time, the result in words and any update found. */
    suspend fun check() {
        if (!enabled || _state.value.busy) return
        _state.update { it.copy(checking = true, error = null) }
        val now = clock()
        var found: AvailableUpdate? = _state.value.available
        var error: String? = null
        val result = try {
            when (val c = source.latest()) {
                UpdateCheck.NoRelease -> {
                    found = null
                    "No app release has been published yet."
                }
                is UpdateCheck.UpToDate -> {
                    found = null
                    "Up to date (latest is v${c.latestVersion})."
                }
                is UpdateCheck.Available -> {
                    found = c.update
                    "Update available: v${c.update.version}."
                }
            }
        } catch (e: CancellationException) {
            _state.update { it.copy(checking = false) }
            throw e
        } catch (e: UpdateException) {
            error = e.message ?: "The update check failed."
            "Check failed: $error"
        } catch (e: Exception) {
            error = "The update check failed (${e.javaClass.simpleName})."
            "Check failed: $error"
        }
        val pending = found?.let { UpdateJson.encodeToString(AvailableUpdate.serializer(), it) }.orEmpty()
        settings.update { it.copy(updateLastCheckAt = now, updateLastResult = result, updatePending = pending) }
        _state.update { it.copy(checking = false, available = found, error = error) }
    }

    suspend fun install() {
        val u = _state.value.available ?: return
        if (_state.value.busy) return
        if (!installer.canInstall()) {
            _state.update { it.copy(permissionNeeded = true, error = null) }
            return
        }
        _state.update { it.copy(permissionNeeded = false, error = null, download = DownloadProgress(0, u.size)) }
        try {
            var lastPercent = -1
            val file = source.download(u) { done, total ->
                val pct = if (total > 0) (done * 100 / total).toInt() else 0
                if (pct != lastPercent) {
                    lastPercent = pct
                    _state.update { it.copy(download = DownloadProgress(done, total)) }
                }
            }
            installer.install(file)
            _state.update { it.copy(download = null) }
        } catch (e: CancellationException) {
            _state.update { it.copy(download = null) }
            throw e
        } catch (e: UpdateException) {
            _state.update { it.copy(download = null, error = e.message ?: "The update failed.") }
        } catch (e: Exception) {
            _state.update { it.copy(download = null, error = "The update failed (${e.javaClass.simpleName}). Try again.") }
        }
    }

    private suspend fun restorePending() {
        val raw = settings.flow.first().updatePending
        if (raw.isBlank()) return
        val u = try {
            UpdateJson.decodeFromString(AvailableUpdate.serializer(), raw)
        } catch (_: SerializationException) {
            null
        }
        // After the app was updated the stored update is no longer newer than the running version.
        if (u != null && u.versionCode > currentVersionCode) {
            _state.update { it.copy(available = u) }
        } else {
            settings.update { it.copy(updatePending = "") }
        }
    }

    companion object {
        const val CHECK_INTERVAL_MS = 6L * 60 * 60 * 1000
    }
}
