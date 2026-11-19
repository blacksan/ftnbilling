package ke.co.ftw.ftnadmin.data.update

import java.io.File
import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.Json
import okhttp3.HttpUrl

/** `update.json`, the release asset the workflow writes next to the APK. */
@Serializable
data class UpdateInfo(
    val version: String = "",
    val versionCode: Long = 0,
    /** File name of the APK asset in the same release. */
    val apk: String = "",
    val sha256: String = "",
    val size: Long = 0,
    val notes: String = "",
)

/** A newer release that passed validation and is ready to download. */
@Serializable
data class AvailableUpdate(
    val version: String,
    val versionCode: Long,
    val apkName: String,
    val apkUrl: String,
    val sha256: String,
    val size: Long,
    val notes: String = "",
)

@Serializable
internal data class GhAsset(
    val name: String = "",
    @SerialName("browser_download_url") val url: String = "",
)

@Serializable
internal data class GhRelease(
    @SerialName("tag_name") val tag: String = "",
    val draft: Boolean = false,
    val prerelease: Boolean = false,
    @SerialName("published_at") val publishedAt: String? = null,
    @SerialName("created_at") val createdAt: String? = null,
    val assets: List<GhAsset> = emptyList(),
)

sealed interface UpdateCheck {
    /** No `android-v*` release has been published yet. */
    data object NoRelease : UpdateCheck

    data class UpToDate(val latestVersion: String) : UpdateCheck

    data class Available(val update: AvailableUpdate) : UpdateCheck
}

/** A failure whose [message] is already plain words for the user. */
class UpdateException(message: String, cause: Throwable? = null) : Exception(message, cause)

/** Package name, version and signing certificates (SHA-256 hex) of an installed app or an APK file. */
data class ApkIdentity(val packageName: String, val versionCode: Long, val signers: Set<String>)

/** Reads identities through the platform; faked in JVM tests. */
interface ApkInspector {
    /** This app as installed, or null if Android cannot say. */
    fun installed(): ApkIdentity?

    /** The APK at [file], or null if it is not a readable package. */
    fun archive(file: File): ApkIdentity?
}

/** Hands a verified APK to the system installer. */
interface ApkInstaller {
    fun canInstall(): Boolean

    /** @throws UpdateException when no installer can open the file. */
    fun install(file: File)

    /** Opens the "install unknown apps" screen for this app. */
    fun openPermissionSettings()
}

/** The network and file side of updating. [UpdateManager] drives it. */
interface UpdateSource {
    suspend fun latest(): UpdateCheck

    /** Downloads, verifies size, SHA-256 and signing certificate, and returns the APK. */
    suspend fun download(update: AvailableUpdate, onProgress: (done: Long, total: Long) -> Unit): File

    /** Deletes APKs left in the cache by earlier downloads. */
    fun cleanup()
}

/** Which URLs the update code may fetch: https only, and only these hosts. */
class UpdatePolicy(private val hosts: Set<String>, private val httpsOnly: Boolean = true) {
    fun allows(url: HttpUrl): Boolean =
        (!httpsOnly || url.isHttps) && url.username.isEmpty() && url.password.isEmpty() && url.host in hosts

    companion object {
        val Production = UpdatePolicy(setOf("github.com", "objects.githubusercontent.com", "release-assets.githubusercontent.com"))
    }
}

internal val UpdateJson = Json {
    ignoreUnknownKeys = true
    coerceInputValues = true
}

/** Release notes as short plain lines: no Markdown headings or bold, bullets as dots. */
fun cleanNotes(raw: String, maxLines: Int = 6): String =
    raw.lineSequence()
        .map { it.replace("**", "").trim().trimStart('#').trim() }
        .filter { it.isNotEmpty() && !it.startsWith("Full Changelog") }
        .map { if (it.startsWith("* ") || it.startsWith("- ")) "• " + it.drop(2) else it }
        .take(maxLines)
        .joinToString("\n")
