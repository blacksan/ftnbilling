package ke.co.ftw.ftnadmin.data.update

import java.io.File
import java.security.MessageDigest
import java.util.jar.JarFile

/**
 * Reads who signed an APK from its v1 (JAR) signature with plain java.util.jar. Android 8.0/8.1 often
 * return no signatures from PackageManager.getPackageArchiveInfo, so this is the fallback there.
 *
 * Returns the SHA-256 (hex) of each signer's certificate, or an empty set when the file has no v1
 * signature, an entry is unsigned or does not match its digest, or the file cannot be read.
 */
object V1Signers {
    fun read(file: File): Set<String> = try {
        JarFile(file, true).use { jar -> readAll(jar) }
    } catch (_: Exception) {
        emptySet()
    }

    private fun readAll(jar: JarFile): Set<String> {
        var common: Set<String>? = null
        val buf = ByteArray(8192)
        for (entry in jar.entries()) {
            if (entry.isDirectory || entry.name.startsWith("META-INF/")) continue
            // Reading to the end makes the JDK check the entry against its signed digest.
            jar.getInputStream(entry).use { input ->
                while (input.read(buf) >= 0) {
                    // discard
                }
            }
            val signers = entry.codeSigners ?: return emptySet()
            val certs = signers.map { sha256Hex(it.signerCertPath.certificates.first().encoded) }.toSet()
            if (certs.isEmpty()) return emptySet()
            if (common != null && common != certs) return emptySet()
            common = certs
        }
        return common.orEmpty()
    }

    private fun sha256Hex(bytes: ByteArray): String =
        MessageDigest.getInstance("SHA-256").digest(bytes).joinToString("") { "%02x".format(it) }
}
