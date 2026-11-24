package ke.co.ftw.ftnadmin.ui

import androidx.datastore.preferences.core.PreferenceDataStoreFactory
import ke.co.ftw.ftnadmin.core.network.ApiClient
import ke.co.ftw.ftnadmin.core.network.MemoryTokenStore
import ke.co.ftw.ftnadmin.core.network.Session
import ke.co.ftw.ftnadmin.core.network.SessionManager
import ke.co.ftw.ftnadmin.core.network.UserInfo
import ke.co.ftw.ftnadmin.core.prefs.AppSettings
import ke.co.ftw.ftnadmin.core.prefs.Settings
import ke.co.ftw.ftnadmin.core.prefs.ThemeMode
import ke.co.ftw.ftnadmin.data.routers.RouterContext
import ke.co.ftw.ftnadmin.data.sites.SitesRepository
import ke.co.ftw.ftnadmin.ui.common.Loadable
import java.io.File
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.test.UnconfinedTestDispatcher
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder

/** Theme preference (System/Light/Dark) and the persisted router selection, on a real DataStore file. */
@OptIn(ExperimentalCoroutinesApi::class)
class ThemeAndRouterContextTest {
    /** A folder that is unique to each test and deleted after it, so no two tests ever share a DataStore file. */
    @get:Rule val tmp = TemporaryFolder()

    // Unconfined test dispatcher: DataStore writes, the selection flow and the repository calls run on the
    // calling thread instead of racing on Dispatchers.IO, and nothing runs after [tearDown] cancels the scope.
    private val dispatcher = UnconfinedTestDispatcher()
    private val scope = CoroutineScope(SupervisorJob() + dispatcher)
    private lateinit var file: File
    private lateinit var settings: Settings

    @Before fun setUp() {
        file = File(tmp.newFolder(), "settings.preferences_pb")
        settings = Settings(PreferenceDataStoreFactory.create(scope = scope, produceFile = { file }), scope)
    }

    @After fun tearDown() {
        // Cancel and wait for the scope's children (DataStore actor, stateIn collectors) before the folder is removed.
        scope.cancel()
        runBlocking { scope.coroutineContext[Job]?.join() }
    }

    private fun eventually(what: String, cond: () -> Boolean) {
        val end = System.currentTimeMillis() + 20_000
        while (!cond() && System.currentTimeMillis() < end) Thread.sleep(20)
        assertTrue(what, cond())
    }

    @Test fun themeModeResolvesLightDarkAndFollowsTheSystem() {
        assertTrue(ThemeMode.Dark.isDark(systemDark = false))
        assertFalse(ThemeMode.Light.isDark(systemDark = true))
        assertTrue(ThemeMode.System.isDark(systemDark = true))
        assertFalse(ThemeMode.System.isDark(systemDark = false))
    }

    @Test fun themeModeKeysRoundTripAndUnknownMeansSystem() {
        ThemeMode.entries.forEach { assertEquals(it, ThemeMode.fromKey(it.key)) }
        assertEquals(ThemeMode.System, ThemeMode.fromKey(null))
        assertEquals(ThemeMode.System, ThemeMode.fromKey("sepia"))
        assertEquals(ThemeMode.System, AppSettings().themeMode)
    }

    @Test fun chosenThemeIsSavedAndEmittedAtRuntime() = runBlocking {
        assertEquals(ThemeMode.System, settings.flow.first().themeMode)
        settings.update { it.copy(themeMode = ThemeMode.Dark) }
        assertEquals(ThemeMode.Dark, settings.flow.first().themeMode)
        settings.update { it.copy(themeMode = ThemeMode.Light) }
        assertEquals(ThemeMode.Light, settings.flow.first().themeMode)
        // Other settings are untouched by a theme change.
        assertTrue(settings.flow.first().flagSecure)
    }

    @Test fun themeAndRouterChoicesAreWrittenToDisk() = runBlocking {
        settings.update { it.copy(themeMode = ThemeMode.Dark, selectedRouterId = "r1") }
        val read = settings.flow.first()
        assertEquals(ThemeMode.Dark, read.themeMode)
        assertEquals("r1", read.selectedRouterId)
        eventually("saved to disk") { file.exists() && file.length() > 0 }
    }

    @Test fun selectedRouterIsPersistedAndFallsBackToAllWhenItDisappears() {
        val server = MockWebServer()
        server.start()
        try {
            val sessions = SessionManager(MemoryTokenStore(Session("a", "", "r", "", "d", UserInfo("1", "b", "owner"))))
            sessions.restore()
            val api = ApiClient.create(server.url("/").toString(), sessions, io = dispatcher)
            val ctx = RouterContext(SitesRepository(api), settings, scope)
            assertNull(ctx.selection.value)

            ctx.select("r1")
            eventually("selection saved") { ctx.selection.value == "r1" }

            server.enqueue(MockResponse().setBody("""{"items":[{"router_id":"r1","router_name":"Kilimani","online":true},{"id":"r2","name":"Lavington"}]}"""))
            ctx.refresh()
            eventually("routers loaded") { ctx.routers.value is Loadable.Ready }
            assertEquals("r1", ctx.selection.value)
            assertEquals("Kilimani", ctx.selectedRouter()?.label)

            // The saved router is gone from the server: back to "All routers".
            server.enqueue(MockResponse().setBody("""{"items":[{"id":"r2","name":"Lavington"}]}"""))
            ctx.refresh()
            eventually("fallback to all") { ctx.selection.value == null }
            assertNull(ctx.selectedRouter())

            ctx.select("r2")
            eventually("r2 selected") { ctx.selection.value == "r2" }
            ctx.select(null)
            eventually("all selected") { ctx.selection.value == null }
            // ...and "All routers" is what was written to disk, not just what the flow last showed.
            assertEquals("", runBlocking { settings.flow.first().selectedRouterId })
        } finally {
            server.shutdown()
        }
    }
}
