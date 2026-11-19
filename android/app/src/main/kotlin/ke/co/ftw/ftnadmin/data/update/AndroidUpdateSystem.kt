package ke.co.ftw.ftnadmin.data.update

import android.content.ActivityNotFoundException
import android.content.Context
import android.content.Intent
import android.content.pm.PackageInfo
import android.content.pm.PackageManager
import android.os.Build
import android.provider.Settings
import androidx.core.content.FileProvider
import androidx.core.net.toUri
import java.io.File
import java.security.MessageDigest

/** Reads package identity and signing certificates through [PackageManager]. */
class AndroidApkInspector(private val context: Context) : ApkInspector {
    private val pm: PackageManager get() = context.packageManager

    override fun installed(): ApkIdentity? = try {
        identity(packageInfo(context.packageName))
    } catch (_: PackageManager.NameNotFoundException) {
        null
    }

    override fun archive(file: File): ApkIdentity? {
        val id = archiveInfo(file.path)?.let(::identity) ?: return null
        // Before Android 9 the platform often returns no signatures for an APK that is not installed:
        // read them from the v1 (JAR) signature instead.
        return if (id.signers.isEmpty() && Build.VERSION.SDK_INT < Build.VERSION_CODES.P) {
            id.copy(signers = V1Signers.read(file))
        } else {
            id
        }
    }

    @Suppress("DEPRECATION")
    private fun packageInfo(name: String): PackageInfo =
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            pm.getPackageInfo(name, PackageManager.PackageInfoFlags.of(FLAGS.toLong()))
        } else {
            pm.getPackageInfo(name, FLAGS)
        }

    @Suppress("DEPRECATION")
    private fun archiveInfo(path: String): PackageInfo? =
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            pm.getPackageArchiveInfo(path, PackageManager.PackageInfoFlags.of(FLAGS.toLong()))
        } else {
            pm.getPackageArchiveInfo(path, FLAGS)
        }

    @Suppress("DEPRECATION")
    private fun identity(info: PackageInfo): ApkIdentity {
        val certs: List<ByteArray> = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.P) {
            info.signingInfo?.apkContentsSigners?.map { it.toByteArray() }.orEmpty()
        } else {
            info.signatures?.map { it.toByteArray() }.orEmpty()
        }
        val code = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.P) info.longVersionCode else info.versionCode.toLong()
        return ApkIdentity(info.packageName.orEmpty(), code, certs.map(::sha256Hex).toSet())
    }

    private fun sha256Hex(bytes: ByteArray): String =
        MessageDigest.getInstance("SHA-256").digest(bytes).joinToString("") { "%02x".format(it) }

    private companion object {
        /** GET_SIGNING_CERTIFICATES from API 28, GET_SIGNATURES below (minSdk is 26). */
        @Suppress("DEPRECATION")
        val FLAGS: Int = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.P) {
            PackageManager.GET_SIGNING_CERTIFICATES
        } else {
            PackageManager.GET_SIGNATURES
        }
    }
}

/** Opens a downloaded APK in the system installer through a FileProvider URI. */
class AndroidApkInstaller(private val context: Context) : ApkInstaller {
    override fun canInstall(): Boolean = context.packageManager.canRequestPackageInstalls()

    override fun install(file: File) {
        val uri = FileProvider.getUriForFile(context, "${context.packageName}.updates", file)
        val intent = Intent(Intent.ACTION_VIEW)
            .setDataAndType(uri, APK_MIME)
            .addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION or Intent.FLAG_ACTIVITY_NEW_TASK)
        try {
            context.startActivity(intent)
        } catch (e: ActivityNotFoundException) {
            throw UpdateException("This phone has no app that can install updates.", e)
        }
    }

    override fun openPermissionSettings() {
        val intent = Intent(Settings.ACTION_MANAGE_UNKNOWN_APP_SOURCES, "package:${context.packageName}".toUri())
            .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
        try {
            context.startActivity(intent)
        } catch (_: ActivityNotFoundException) {
            context.startActivity(Intent(Settings.ACTION_SECURITY_SETTINGS).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK))
        }
    }

    private companion object {
        const val APK_MIME = "application/vnd.android.package-archive"
    }
}
