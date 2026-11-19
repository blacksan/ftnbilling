package ke.co.ftw.ftnadmin.data.update

import java.io.File
import java.security.MessageDigest
import java.util.Collections
import kotlinx.coroutines.runBlocking
import okhttp3.HttpUrl.Companion.toHttpUrl
import okhttp3.OkHttpClient
import okhttp3.mockwebserver.Dispatcher
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import okhttp3.mockwebserver.RecordedRequest
import okio.Buffer
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Before
import org.junit.Test

class FakeInspector(var installed: ApkIdentity?, var archive: ApkIdentity?) : ApkInspector {
    override fun installed() = installed

    override fun archive(file: File) = archive
}

class UpdateRepositoryTest {
    private lateinit var server: MockWebServer
    private lateinit var dir: File
    private val routes = HashMap<String, () -> MockResponse>()
    private val requested: MutableList<String> = Collections.synchronizedList(mutableListOf())
    private val apk = ByteArray(150_000) { (it % 251).toByte() }
    private val apkSha = sha256(apk)
    private val pkg = "ke.co.ftw.ftnadmin"
    private val signer = "ab".repeat(32)

    @Before fun setUp() {
        server = MockWebServer()
        server.dispatcher = object : Dispatcher() {
            override fun dispatch(request: RecordedRequest): MockResponse {
                val full = request.path.orEmpty()
                val path = full.substringBefore('?')
                requested += path
                return (routes[full] ?: routes[path])?.invoke() ?: MockResponse().setResponseCode(404)
            }
        }
        server.start()
        dir = File.createTempFile("ftn-updates", "").also {
            it.delete()
            it.mkdirs()
        }
    }

    @After fun tearDown() {
        server.shutdown()
        dir.deleteRecursively()
    }

    private fun url(path: String) = server.url(path).toString()

    private fun repo(
        currentCode: Long = 10,
        inspector: ApkInspector = FakeInspector(ApkIdentity(pkg, 11, setOf(signer)), ApkIdentity(pkg, 11, setOf(signer))),
        policy: UpdatePolicy = UpdatePolicy(setOf(server.url("/").host), httpsOnly = false),
        clock: () -> Long = System::currentTimeMillis,
    ) = UpdateRepository(
        http = OkHttpClient(),
        currentVersionCode = currentCode,
        currentVersionName = "0.1.0",
        packageName = pkg,
        updatesDir = dir,
        inspector = inspector,
        apiBase = server.url("/"),
        policy = policy,
        clock = clock,
    )

    private fun release(tag: String, published: String, draft: Boolean = false, pre: Boolean = false, infoUrl: String? = null): String {
        val ver = tag.substringAfter("v")
        return """{"tag_name":"$tag","draft":$draft,"prerelease":$pre,"published_at":"$published","assets":[
            {"name":"update.json","browser_download_url":"${infoUrl ?: url("/dl/$tag/update.json")}"},
            {"name":"ftn-admin-$ver.apk","browser_download_url":"${url("/dl/$tag/ftn-admin-$ver.apk")}"}]}"""
    }

    private fun serveReleases(vararg items: String) {
        routes["/repos/blacksan/ftnbilling/releases"] = { MockResponse().setBody("[${items.joinToString(",")}]") }
    }

    private fun serveInfo(tag: String, code: Long, sha: String = apkSha, size: Long = apk.size.toLong()) {
        val ver = tag.substringAfter("v")
        routes["/dl/$tag/update.json"] = {
            MockResponse().setBody("""{"version":"$ver","versionCode":$code,"apk":"ftn-admin-$ver.apk","sha256":"$sha","size":$size,"notes":"## Changes\n* thing"}""")
        }
    }

    private fun available(path: String = "/dl/apk", sha: String = apkSha, size: Long = apk.size.toLong()) = AvailableUpdate(
        version = "0.2.0", versionCode = 11, apkName = "ftn-admin-0.2.0.apk", apkUrl = url(path), sha256 = sha, size = size,
    )

    private fun serveApk(path: String = "/dl/apk") {
        routes[path] = { MockResponse().setBody(Buffer().write(apk)) }
    }

    @Test fun picksNewestPublishedAndroidReleaseAmongMixedTags() = runBlocking {
        serveReleases(
            release("v1.9.0", "2026-11-18T10:00:00Z"), // the server's release: never an app update
            release("android-v0.3.0", "2026-11-17T10:00:00Z", draft = true),
            release("android-v0.2.5", "2026-11-16T10:00:00Z", pre = true),
            release("android-v0.2.0", "2026-11-10T10:00:00Z"),
            release("android-v0.1.0", "2026-10-01T10:00:00Z"),
        )
        serveInfo("android-v0.2.0", code = 11)
        val result = repo().latest()
        assertTrue(result is UpdateCheck.Available)
        val u = (result as UpdateCheck.Available).update
        assertEquals("0.2.0", u.version)
        assertEquals(11L, u.versionCode)
        assertEquals(url("/dl/android-v0.2.0/ftn-admin-0.2.0.apk"), u.apkUrl)
        assertTrue(requested.none { it.contains("v1.9.0") || it.contains("0.3.0") || it.contains("0.2.5") })
    }

    @Test fun looksOnLaterPagesWhenServerReleasesFillTheFirstOne() = runBlocking {
        val filler = (1..100).map { release("v1.$it.0", "2026-11-18T10:00:00Z") }
        routes["/repos/blacksan/ftnbilling/releases?per_page=100&page=1"] = { MockResponse().setBody("[${filler.joinToString(",")}]") }
        routes["/repos/blacksan/ftnbilling/releases?per_page=100&page=2"] = {
            MockResponse().setBody("[${release("android-v0.2.0", "2026-10-10T10:00:00Z")}]")
        }
        serveInfo("android-v0.2.0", code = 11)
        val result = repo().latest()
        assertTrue(result is UpdateCheck.Available)
        assertTrue(requested.count { it == "/repos/blacksan/ftnbilling/releases" } == 2)
    }

    @Test fun givesUpAfterThreePages() = runBlocking {
        val filler = (1..100).map { release("v1.$it.0", "2026-11-18T10:00:00Z") }
        routes["/repos/blacksan/ftnbilling/releases"] = { MockResponse().setBody("[${filler.joinToString(",")}]") }
        assertEquals(UpdateCheck.NoRelease, repo().latest())
        assertEquals(3, requested.count { it == "/repos/blacksan/ftnbilling/releases" })
    }

    @Test fun unreadableSignerOffersTheReleasePageInsteadOfAFalseKeyError() = runBlocking {
        serveApk()
        val inspector = FakeInspector(ApkIdentity(pkg, 11, setOf(signer)), ApkIdentity(pkg, 11, emptySet()))
        try {
            repo(inspector = inspector).download(available()) { _, _ -> }
            fail("expected UpdateException")
        } catch (e: UpdateException) {
            assertFalse(e.message!!.contains("different key"))
            assertEquals("https://github.com/blacksan/ftnbilling/releases/tag/android-v0.2.0", e.manualUrl)
        }
        assertEquals(0, dir.list()!!.size)
    }

    @Test fun noAndroidReleaseMeansNoRelease() = runBlocking {
        serveReleases(release("v1.9.0", "2026-11-18T10:00:00Z"), release("v1.8.0", "2026-11-01T10:00:00Z"))
        assertEquals(UpdateCheck.NoRelease, repo().latest())
    }

    @Test fun pickReleaseIgnoresDraftsPrereleasesAndServerTags() {
        val a = GhRelease("android-v1.0.0", publishedAt = "2026-01-01T00:00:00Z")
        val b = GhRelease("android-v1.1.0", publishedAt = "2026-02-01T00:00:00Z")
        val server = GhRelease("v9.9.9", publishedAt = "2026-03-01T00:00:00Z")
        val draft = GhRelease("android-v2.0.0", draft = true, publishedAt = "2026-04-01T00:00:00Z")
        val pre = GhRelease("android-v2.1.0", prerelease = true, publishedAt = "2026-05-01T00:00:00Z")
        assertEquals(b, UpdateRepository.pickRelease(listOf(server, draft, pre, a, b)))
        assertNull(UpdateRepository.pickRelease(listOf(server, draft, pre)))
    }

    @Test fun updateOnlyWhenVersionCodeIsHigher() = runBlocking {
        serveReleases(release("android-v0.2.0", "2026-11-10T10:00:00Z"))
        serveInfo("android-v0.2.0", code = 11)
        assertTrue(repo(currentCode = 10).latest() is UpdateCheck.Available)
        assertEquals(UpdateCheck.UpToDate("0.2.0"), repo(currentCode = 11).latest())
        assertEquals(UpdateCheck.UpToDate("0.2.0"), repo(currentCode = 12).latest())
    }

    @Test fun malformedUpdateJsonIsRejected() = runBlocking {
        serveReleases(release("android-v0.2.0", "2026-11-10T10:00:00Z"))
        serveInfo("android-v0.2.0", code = 11, sha = "nothex")
        try {
            repo().latest()
            fail("expected UpdateException")
        } catch (e: UpdateException) {
            assertTrue(e.message!!.contains("not valid"))
        }
    }

    @Test fun updateJsonOnDisallowedHostIsRefused() = runBlocking {
        serveReleases(release("android-v0.2.0", "2026-11-10T10:00:00Z", infoUrl = "https://evil.example/update.json"))
        try {
            repo().latest()
            fail("expected UpdateException")
        } catch (e: UpdateException) {
            assertTrue(e.message!!.contains("Refused"))
        }
    }

    @Test fun rateLimitIsExplainedInWords() = runBlocking {
        routes["/repos/blacksan/ftnbilling/releases"] = { MockResponse().setResponseCode(403).setHeader("X-RateLimit-Remaining", "0") }
        try {
            repo().latest()
            fail("expected UpdateException")
        } catch (e: UpdateException) {
            assertTrue(e.message!!.contains("limiting"))
        }
    }

    @Test fun downloadVerifiesAndKeepsTheApk() = runBlocking {
        serveApk()
        var last = 0L
        val f = repo().download(available()) { done, _ -> last = done }
        assertTrue(f.readBytes().contentEquals(apk))
        assertEquals(apk.size.toLong(), last)
        assertEquals(listOf("ftn-admin-0.2.0.apk"), dir.list()!!.toList())
    }

    @Test fun sha256MismatchIsRefusedAndNothingIsKept() = runBlocking {
        serveApk()
        try {
            repo().download(available(sha = "0".repeat(64))) { _, _ -> }
            fail("expected UpdateException")
        } catch (e: UpdateException) {
            assertTrue(e.message!!.contains("SHA-256"))
        }
        assertEquals(0, dir.list()!!.size)
    }

    @Test fun sizeMismatchIsRefused() = runBlocking {
        serveApk()
        try {
            repo().download(available(size = apk.size + 1L)) { _, _ -> }
            fail("expected UpdateException")
        } catch (e: UpdateException) {
            assertTrue(e.message!!.contains("bytes"))
        }
        assertEquals(0, dir.list()!!.size)
    }

    @Test fun differentSigningCertificateIsRefused() = runBlocking {
        serveApk()
        val inspector = FakeInspector(ApkIdentity(pkg, 11, setOf(signer)), ApkIdentity(pkg, 11, setOf("cd".repeat(32))))
        try {
            repo(inspector = inspector).download(available()) { _, _ -> }
            fail("expected UpdateException")
        } catch (e: UpdateException) {
            assertTrue(e.message!!.contains("different key"))
        }
        assertEquals(0, dir.list()!!.size)
    }

    @Test fun otherPackageOrVersionIsRefused() = runBlocking {
        serveApk()
        for (bad in listOf(ApkIdentity("evil.app", 11, setOf(signer)), ApkIdentity(pkg, 5, setOf(signer)))) {
            try {
                repo(inspector = FakeInspector(ApkIdentity(pkg, 10, setOf(signer)), bad)).download(available()) { _, _ -> }
                fail("expected UpdateException")
            } catch (e: UpdateException) {
                assertNotNull(e.message)
            }
        }
        assertEquals(0, dir.list()!!.size)
    }

    @Test fun redirectToDisallowedHostIsRefused() = runBlocking {
        routes["/dl/redir"] = { MockResponse().setResponseCode(302).setHeader("Location", "https://evil.example/app.apk") }
        try {
            repo().download(available(path = "/dl/redir")) { _, _ -> }
            fail("expected UpdateException")
        } catch (e: UpdateException) {
            assertTrue(e.message!!.contains("Refused"))
        }
        assertEquals(0, dir.list()!!.size)
    }

    @Test fun redirectToAllowedHostIsFollowed() = runBlocking {
        serveApk("/dl/final")
        routes["/dl/hop"] = { MockResponse().setResponseCode(302).setHeader("Location", url("/dl/final")) }
        val f = repo().download(available(path = "/dl/hop")) { _, _ -> }
        assertTrue(f.readBytes().contentEquals(apk))
    }

    @Test fun productionPolicyAllowsOnlyGithubHostsOverHttps() {
        val p = UpdatePolicy.Production
        assertTrue(p.allows("https://github.com/blacksan/ftnbilling/releases/download/android-v1.0.0/update.json".toHttpUrl()))
        assertTrue(p.allows("https://objects.githubusercontent.com/github-production-release-asset/1".toHttpUrl()))
        assertTrue(p.allows("https://release-assets.githubusercontent.com/github-production-release-asset/1".toHttpUrl()))
        assertFalse(p.allows("http://github.com/x".toHttpUrl()))
        assertFalse(p.allows("https://evil.example/x".toHttpUrl()))
        assertFalse(p.allows("https://github.com.evil.example/x".toHttpUrl()))
        assertFalse(p.allows("https://notgithub.com/x".toHttpUrl()))
        assertFalse(p.allows("https://githubusercontent.com/x".toHttpUrl()))
        assertFalse(p.allows("https://raw.githubusercontent.com/x".toHttpUrl()))
        assertFalse(p.allows("https://api.github.com/repos/x".toHttpUrl()))
        assertFalse(p.allows("https://github.com@evil.example/x".toHttpUrl()))
        assertFalse(p.allows("https://user:pw@github.com/x".toHttpUrl()))
    }

    @Test fun cleanupRemovesPartialStaleAndInstalledVersionFiles() {
        val now = 10L * 24 * 3600 * 1000
        val part = File(dir, "ftn-admin-0.3.0.apk.part").also { it.writeText("x") }
        val running = File(dir, "ftn-admin-0.1.0.apk").also { it.writeText("x") }
        val old = File(dir, "ftn-admin-0.2.0.apk").also {
            it.writeText("x")
            it.setLastModified(now - 2 * 24 * 3600 * 1000L)
        }
        val fresh = File(dir, "ftn-admin-0.4.0.apk").also {
            it.writeText("x")
            it.setLastModified(now - 1000)
        }
        repo(clock = { now }).cleanup()
        assertFalse(part.exists())
        assertFalse(running.exists())
        assertFalse(old.exists())
        assertTrue(fresh.exists())
    }

    private fun sha256(b: ByteArray) = MessageDigest.getInstance("SHA-256").digest(b).joinToString("") { "%02x".format(it) }
}
