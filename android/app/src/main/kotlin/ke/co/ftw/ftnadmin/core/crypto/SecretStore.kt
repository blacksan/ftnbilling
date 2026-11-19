package ke.co.ftw.ftnadmin.core.crypto

import android.content.Context
import android.util.Log
import java.io.File
import java.io.IOException
import java.security.GeneralSecurityException

/**
 * Small encrypted file store under `noBackupFilesDir/secure/<name>.bin`.
 * Entries are never included in backups or device transfer.
 */
class SecretStore(context: Context, private val cipher: AeadCipher) {
    private val dir = File(context.noBackupFilesDir, "secure")

    @Synchronized
    fun put(name: String, value: ByteArray) {
        val target = file(name)
        if (!dir.isDirectory && !dir.mkdirs()) throw IOException("cannot create secure directory")
        val tmp = File(dir, "$name.tmp")
        tmp.writeBytes(SecretBlob.seal(cipher, name, value))
        if (!tmp.renameTo(target)) {
            tmp.delete()
            throw IOException("cannot replace $name")
        }
    }

    /** Returns null when absent or unreadable (for example the key was lost); unreadable entries are deleted. */
    @Synchronized
    fun get(name: String): ByteArray? {
        val f = file(name)
        if (!f.isFile) return null
        return try {
            SecretBlob.open(cipher, name, f.readBytes())
        } catch (e: GeneralSecurityException) {
            Log.w(TAG, "discarding unreadable secret '$name'", e)
            f.delete()
            null
        }
    }

    @Synchronized
    fun delete(name: String) {
        file(name).delete()
    }

    /** Deletes every entry and the Keystore key. */
    @Synchronized
    fun wipeAll() {
        dir.deleteRecursively()
        cipher.destroyKey()
    }

    private fun file(name: String): File {
        require(NAME.matches(name)) { "bad secret name" }
        return File(dir, "$name.bin")
    }

    private companion object {
        const val TAG = "SecretStore"
        val NAME = Regex("[a-z0-9_]{1,32}")
    }
}
