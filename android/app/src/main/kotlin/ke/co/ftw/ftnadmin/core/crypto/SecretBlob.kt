package ke.co.ftw.ftnadmin.core.crypto

import java.security.GeneralSecurityException

/**
 * On-disk framing for an encrypted secret: `0x01 | 12-byte IV | ciphertext+tag`.
 * The entry name is bound as AAD so blobs cannot be swapped between entries.
 */
object SecretBlob {
    const val VERSION: Byte = 1
    const val IV_SIZE = 12
    private const val TAG_SIZE = 16

    fun seal(cipher: AeadCipher, name: String, plaintext: ByteArray): ByteArray {
        val sealed = cipher.encrypt(plaintext, name.toByteArray(Charsets.UTF_8))
        check(sealed.iv.size == IV_SIZE) { "unexpected IV size ${sealed.iv.size}" }
        return byteArrayOf(VERSION) + sealed.iv + sealed.ciphertext
    }

    @Throws(GeneralSecurityException::class)
    fun open(cipher: AeadCipher, name: String, blob: ByteArray): ByteArray {
        if (blob.size < 1 + IV_SIZE + TAG_SIZE) throw GeneralSecurityException("blob too short")
        if (blob[0] != VERSION) throw GeneralSecurityException("unsupported blob version ${blob[0]}")
        val iv = blob.copyOfRange(1, 1 + IV_SIZE)
        val ciphertext = blob.copyOfRange(1 + IV_SIZE, blob.size)
        return cipher.decrypt(iv, ciphertext, name.toByteArray(Charsets.UTF_8))
    }
}
