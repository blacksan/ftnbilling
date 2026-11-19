package ke.co.ftw.ftnadmin.data.update

import java.io.File
import java.io.IOException
import java.net.SocketTimeoutException
import java.net.UnknownHostException
import java.security.MessageDigest
import javax.net.ssl.SSLException
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.currentCoroutineContext
import kotlinx.coroutines.ensureActive
import kotlinx.coroutines.withContext
import kotlinx.serialization.SerializationException
import kotlinx.serialization.builtins.ListSerializer
import okhttp3.HttpUrl
import okhttp3.HttpUrl.Companion.toHttpUrl
import okhttp3.HttpUrl.Companion.toHttpUrlOrNull
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.Response

/**
 * Looks for a newer release of the app on GitHub (public, no auth) and downloads it.
 *
 * [http] must be a client WITHOUT the app's auth interceptors: nothing of the admin session may be
 * sent to GitHub. Redirects are followed here, by hand, so every hop is checked against [policy].
 */
class UpdateRepository(
    http: OkHttpClient,
    private val currentVersionCode: Long,
    private val currentVersionName: String,
    private val packageName: String,
    private val updatesDir: File,
    private val inspector: ApkInspector,
    private val apiBase: HttpUrl = "https://api.github.com".toHttpUrl(),
    private val repo: String = "blacksan/ftnbilling",
    private val policy: UpdatePolicy = UpdatePolicy.Production,
    private val clock: () -> Long = System::currentTimeMillis,
) : UpdateSource {
    private val client = http.newBuilder().followRedirects(false).followSslRedirects(false).build()

    override suspend fun latest(): UpdateCheck {
        val listUrl = apiBase.newBuilder().addPathSegments("repos/$repo/releases").addQueryParameter("per_page", "20").build()
        val releasesText = fetchText(listUrl, trusted = true, accept = "application/vnd.github+json")
        val releases = try {
            UpdateJson.decodeFromString(ListSerializer(GhRelease.serializer()), releasesText)
        } catch (e: SerializationException) {
            throw UpdateException("GitHub sent a releases list the app cannot read.", e)
        }
        val release = pickRelease(releases) ?: return UpdateCheck.NoRelease

        val infoAsset = release.assets.firstOrNull { it.name == INFO_ASSET }
            ?: throw UpdateException("The newest release (${release.tag}) has no $INFO_ASSET file, so the app cannot tell what it contains.")
        val infoUrl = infoAsset.url.toHttpUrlOrNull()
        if (infoUrl == null || !policy.allows(infoUrl)) throw refused(infoAsset.url)
        val info = try {
            UpdateJson.decodeFromString(UpdateInfo.serializer(), fetchText(infoUrl, trusted = false, accept = "application/json"))
        } catch (e: SerializationException) {
            throw UpdateException("The $INFO_ASSET of ${release.tag} cannot be read.", e)
        }
        validate(info, release.tag)

        if (info.versionCode <= currentVersionCode) return UpdateCheck.UpToDate(info.version)

        val apkAsset = release.assets.firstOrNull { it.name == info.apk }
            ?: throw UpdateException("Release ${release.tag} lists ${info.apk} but the file is not attached to it.")
        val apkUrl = apkAsset.url.toHttpUrlOrNull()
        if (apkUrl == null || !policy.allows(apkUrl)) throw refused(apkAsset.url)
        return UpdateCheck.Available(
            AvailableUpdate(
                version = info.version,
                versionCode = info.versionCode,
                apkName = info.apk,
                apkUrl = apkUrl.toString(),
                sha256 = info.sha256.lowercase(),
                size = info.size,
                notes = info.notes,
            ),
        )
    }

    override suspend fun download(update: AvailableUpdate, onProgress: (done: Long, total: Long) -> Unit): File =
        withContext(Dispatchers.IO) {
            val url = update.apkUrl.toHttpUrlOrNull()
            if (url == null || !policy.allows(url)) throw refused(update.apkUrl)
            if (!APK_NAME.matches(update.apkName)) throw UpdateException("The release names its file ${update.apkName}, which is not a valid app file name.")
            clearDir()
            updatesDir.mkdirs()
            val part = File(updatesDir, update.apkName + ".part")
            val out = File(updatesDir, update.apkName)
            val ctx = currentCoroutineContext()
            try {
                withResponse(url, trusted = false, accept = "application/octet-stream") { r ->
                    val body = r.body ?: throw UpdateException("GitHub sent an empty download.")
                    val announced = body.contentLength()
                    if (announced >= 0 && announced != update.size) {
                        throw UpdateException("The download is $announced bytes but the release says ${update.size}. Nothing was installed.")
                    }
                    val digest = MessageDigest.getInstance("SHA-256")
                    var done = 0L
                    body.byteStream().use { input ->
                        part.outputStream().use { sink ->
                            val buf = ByteArray(BUFFER)
                            while (true) {
                                ctx.ensureActive()
                                val n = input.read(buf)
                                if (n < 0) break
                                done += n
                                if (done > update.size) throw UpdateException("The download is larger than the release says. Nothing was installed.")
                                digest.update(buf, 0, n)
                                sink.write(buf, 0, n)
                                onProgress(done, update.size)
                            }
                        }
                    }
                    if (done != update.size) throw UpdateException("The download stopped early ($done of ${update.size} bytes). Try again.")
                    val got = digest.digest().toHex()
                    if (!got.equals(update.sha256, ignoreCase = true)) {
                        throw UpdateException("The downloaded file does not match the SHA-256 published with the release. It was deleted and nothing was installed.")
                    }
                }
                if (!part.renameTo(out)) throw UpdateException("Could not save the update on this phone.")
                verifyApk(out, update)
                out
            } catch (e: Throwable) {
                part.delete()
                out.delete()
                throw e
            }
        }

    override fun cleanup() {
        val now = clock()
        updatesDir.listFiles()?.forEach { f ->
            val stale = f.name.endsWith(".part") ||
                f.name == "ftn-admin-$currentVersionName.apk" ||
                now - f.lastModified() > MAX_AGE_MS
            if (stale) f.delete()
        }
    }

    private fun clearDir() {
        updatesDir.listFiles()?.forEach { it.delete() }
    }

    private fun verifyApk(file: File, update: AvailableUpdate) {
        val apk = inspector.archive(file)
            ?: throw UpdateException("Android cannot read the downloaded file as an app. It was deleted.")
        if (apk.packageName != packageName) throw UpdateException("The downloaded file is a different app (${apk.packageName}). It was deleted.")
        if (apk.versionCode != update.versionCode) throw UpdateException("The downloaded app has version code ${apk.versionCode}, not ${update.versionCode}. It was deleted.")
        val installed = inspector.installed() ?: throw UpdateException("Android did not tell this app its own signing certificate, so the update cannot be checked.")
        if (apk.signers.isEmpty() || apk.signers != installed.signers) {
            throw UpdateException("The update is signed with a different key than the installed app, so Android would refuse it. It was deleted and nothing was installed.")
        }
    }

    private fun validate(info: UpdateInfo, tag: String) {
        val problem = when {
            !VERSION.matches(info.version) -> "version is not X.Y.Z"
            "android-v${info.version}" != tag -> "version ${info.version} does not match the tag $tag"
            info.versionCode <= 0 -> "versionCode is missing"
            !APK_NAME.matches(info.apk) -> "apk is not a plain file name"
            !SHA256.matches(info.sha256) -> "sha256 is not 64 hex digits"
            info.size <= 0 || info.size > MAX_APK_BYTES -> "size is out of range"
            else -> null
        }
        if (problem != null) throw UpdateException("The $INFO_ASSET of $tag is not valid ($problem).")
    }

    private suspend fun fetchText(url: HttpUrl, trusted: Boolean, accept: String): String =
        withResponse(url, trusted, accept) { r ->
            val body = r.body ?: throw UpdateException("GitHub sent an empty answer.")
            val source = body.source()
            // More than the limit available means the answer is too large to be what we expect.
            if (source.request(MAX_TEXT_BYTES + 1L)) throw UpdateException("GitHub sent an unexpectedly large answer.")
            source.readUtf8()
        }

    /**
     * GET [start], following redirects only to addresses [policy] allows. [trusted] marks the first URL
     * as built by this class (the API address) rather than taken from release data.
     */
    private suspend fun <T> withResponse(start: HttpUrl, trusted: Boolean, accept: String, block: (Response) -> T): T =
        withContext(Dispatchers.IO) {
            var url = start
            var skipCheck = trusted
            for (hop in 0..MAX_REDIRECTS) {
                if (!skipCheck && !policy.allows(url)) throw refused(url.toString())
                skipCheck = false
                val request = Request.Builder().url(url).header("Accept", accept).header("User-Agent", USER_AGENT).build()
                val response = try {
                    client.newCall(request).execute()
                } catch (e: IOException) {
                    throw networkError(e)
                }
                if (response.isRedirect) {
                    val location = response.header("Location")
                    response.close()
                    url = location?.let { url.resolve(it) } ?: throw UpdateException("GitHub sent a redirect without a target.")
                    continue
                }
                return@withContext response.use { r ->
                    if (!r.isSuccessful) throw httpError(r)
                    try {
                        block(r)
                    } catch (e: IOException) {
                        throw networkError(e)
                    }
                }
            }
            throw UpdateException("GitHub redirected too many times.")
        }

    private fun refused(url: String) = UpdateException(
        "Refused to use $url: updates are only taken from github.com, objects.githubusercontent.com or release-assets.githubusercontent.com over https.",
    )

    private fun httpError(r: Response): UpdateException = when {
        r.code == 429 || (r.code == 403 && r.header("X-RateLimit-Remaining") == "0") ->
            UpdateException("GitHub is limiting requests from this network right now. Try again in a while.")
        r.code == 404 -> UpdateException("GitHub says the releases of this app were not found (HTTP 404).")
        else -> UpdateException("GitHub answered with an error (HTTP ${r.code}).")
    }

    private fun networkError(e: IOException): UpdateException = when (e) {
        is UnknownHostException -> UpdateException("Cannot reach GitHub. Check the phone's internet connection.", e)
        is SocketTimeoutException -> UpdateException("GitHub took too long to answer. Try again.", e)
        is SSLException -> UpdateException("The secure connection to GitHub failed. Check the phone's date and time and network.", e)
        else -> UpdateException("The connection to GitHub broke (${e.message ?: e.javaClass.simpleName}). Try again.", e)
    }

    companion object {
        const val INFO_ASSET = "update.json"
        private const val MAX_REDIRECTS = 5
        private const val BUFFER = 64 * 1024
        private const val MAX_TEXT_BYTES = 2 * 1024 * 1024
        private const val MAX_APK_BYTES = 200L * 1024 * 1024
        private const val MAX_AGE_MS = 24L * 60 * 60 * 1000
        private const val USER_AGENT = "ftn-admin-android"
        private val VERSION = Regex("""\d+\.\d+\.\d+""")
        private val APK_NAME = Regex("""ftn-admin-[0-9A-Za-z._-]+\.apk""")
        private val SHA256 = Regex("""[0-9a-fA-F]{64}""")

        /** Newest published release whose tag starts with `android-v`; drafts, prereleases and server `v*` tags are skipped. */
        internal fun pickRelease(releases: List<GhRelease>): GhRelease? =
            releases
                .withIndex()
                .filter { (_, r) -> !r.draft && !r.prerelease && r.tag.startsWith("android-v") }
                .maxWithOrNull(
                    compareBy<IndexedValue<GhRelease>> { it.value.publishedAt ?: it.value.createdAt ?: "" }
                        .thenByDescending { it.index },
                )
                ?.value

        private fun ByteArray.toHex(): String = joinToString("") { "%02x".format(it) }
    }
}
