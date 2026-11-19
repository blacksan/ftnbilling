package ke.co.ftw.ftnadmin.data.update

import androidx.datastore.preferences.core.PreferenceDataStoreFactory
import java.io.File
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeout
import ke.co.ftw.ftnadmin.core.prefs.Settings
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test

private class FakeSource(var next: () -> UpdateCheck = { UpdateCheck.UpToDate("0.1.0") }) : UpdateSource {
    var checks = 0
    var downloads = 0
    var file: File? = null
    var failWith: Exception? = null

    override suspend fun latest(): UpdateCheck {
        checks++
        return next()
    }

    override suspend fun download(update: AvailableUpdate, onProgress: (Long, Long) -> Unit): File {
        downloads++
        failWith?.let { throw it }
        onProgress(update.size, update.size)
        return file!!
    }

    override fun cleanup() = Unit
}

private class FakeInstaller(var allowed: Boolean = true) : ApkInstaller {
    var installed: File? = null
    var settingsOpened = 0

    override fun canInstall() = allowed

    override fun install(file: File) {
        installed = file
    }

    override fun openPermissionSettings() {
        settingsOpened++
    }
}

class UpdateManagerTest {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)
    private lateinit var file: File
    private lateinit var settings: Settings
    private val source = FakeSource()
    private val installer = FakeInstaller()
    private var now = 1_000_000_000L
    private val hour = 3_600_000L
    private val update = AvailableUpdate("0.2.0", 11, "ftn-admin-0.2.0.apk", "https://github.com/x", "a".repeat(64), 100, "notes")

    @Before fun setUp() {
        file = File.createTempFile("ftn-update-settings", ".preferences_pb").also { it.delete() }
        settings = Settings(PreferenceDataStoreFactory.create(scope = scope, produceFile = { file }), scope)
    }

    @After fun tearDown() {
        scope.cancel()
        file.delete()
    }

    private fun manager(enabled: Boolean = true, code: Long = 10) =
        UpdateManager(source, settings, installer, scope, enabled, code, clock = { now })

    @Test fun autoCheckRunsAtMostOncePerSixHours() = runBlocking {
        val m = manager()
        m.autoCheck()
        assertEquals(1, source.checks)
        now += 6 * hour - 1
        m.autoCheck()
        assertEquals("still inside the 6 hours", 1, source.checks)
        now += 1
        m.autoCheck()
        assertEquals("6 hours passed", 2, source.checks)
        m.autoCheck()
        assertEquals(2, source.checks)
    }

    @Test fun throttleSurvivesANewManagerBecauseTheTimeIsPersisted() = runBlocking {
        manager().autoCheck()
        now += hour
        manager().autoCheck()
        assertEquals(1, source.checks)
    }

    @Test fun failedCheckAlsoCountsAndIsExplained() = runBlocking {
        source.next = { throw UpdateException("Cannot reach GitHub. Check the phone's internet connection.") }
        val m = manager()
        m.autoCheck()
        m.autoCheck()
        assertEquals(1, source.checks)
        assertEquals("Cannot reach GitHub. Check the phone's internet connection.", m.state.value.error)
        assertTrue(settings.flow.first().updateLastResult.startsWith("Check failed"))
    }

    @Test fun switchedOffSettingStopsAutomaticChecksButNotTheButton() = runBlocking {
        settings.update { it.copy(updateCheckEnabled = false) }
        val m = manager()
        m.autoCheck()
        assertEquals(0, source.checks)
        m.check()
        assertEquals(1, source.checks)
    }

    @Test fun debugBuildsNeverCheck() = runBlocking {
        val m = manager(enabled = false)
        m.autoCheck()
        m.check()
        assertEquals(0, source.checks)
        assertFalse(m.state.value.enabled)
    }

    @Test fun aClockSetBackDoesNotBlockChecksForever() = runBlocking {
        manager().autoCheck()
        now -= 10 * hour
        manager().autoCheck()
        assertEquals(2, source.checks)
    }

    @Test fun foundUpdateIsShownAndRemembered() = runBlocking {
        source.next = { UpdateCheck.Available(update) }
        val m = manager()
        m.check()
        assertEquals(update, m.state.value.available)
        assertEquals("Update available: v0.2.0.", settings.flow.first().updateLastResult)
        // A fresh process shows it again without asking GitHub.
        val again = manager()
        again.onAppStart()
        withTimeout(5_000) { again.state.first { it.available != null } }
        assertEquals(update, again.state.value.available)
        assertEquals(1, source.checks)
    }

    @Test fun rememberedUpdateIsDroppedOnceTheAppHasIt() = runBlocking {
        source.next = { UpdateCheck.Available(update) }
        manager().check()
        val upgraded = manager(code = 11)
        upgraded.onAppStart()
        withTimeout(5_000) { while (settings.flow.first().updatePending.isNotEmpty()) kotlinx.coroutines.delay(20) }
        assertNull(upgraded.state.value.available)
    }

    @Test fun installAsksForPermissionFirstThenDownloadsAndHandsOver() = runBlocking {
        source.next = { UpdateCheck.Available(update) }
        source.file = File.createTempFile("ftn", ".apk")
        val m = manager()
        m.check()
        installer.allowed = false
        m.install()
        assertTrue(m.state.value.permissionNeeded)
        assertEquals(0, source.downloads)
        installer.allowed = true
        m.install()
        assertEquals(1, source.downloads)
        assertEquals(source.file, installer.installed)
        assertFalse(m.state.value.permissionNeeded)
        assertNull(m.state.value.download)
        source.file!!.delete()
        Unit
    }

    @Test fun downloadErrorIsShownInWordsAndCanBeRetried() = runBlocking {
        source.next = { UpdateCheck.Available(update) }
        val m = manager()
        m.check()
        source.failWith = UpdateException("The downloaded file does not match the SHA-256 published with the release.")
        m.install()
        assertEquals("The downloaded file does not match the SHA-256 published with the release.", m.state.value.error)
        assertNull(m.state.value.download)
        assertEquals(update, m.state.value.available)
    }
}
