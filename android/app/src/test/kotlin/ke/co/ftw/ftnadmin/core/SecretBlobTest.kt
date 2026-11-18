package ke.co.ftw.ftnadmin.core

import java.security.GeneralSecurityException
import ke.co.ftw.ftnadmin.core.crypto.AeadCipher
import ke.co.ftw.ftnadmin.core.crypto.SecretBlob
import org.junit.Test
import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertThrows

class SecretBlobTest {
    /** Not real crypto: XOR plus a tag derived from the AAD, enough to test framing and AAD binding. */
    private class FakeCipher : AeadCipher {
        var lastAad = ByteArray(0)
        var destroyed = false

        override fun encrypt(plaintext: ByteArray, aad: ByteArray): AeadCipher.Sealed {
            lastAad = aad
            val iv = ByteArray(12) { it.toByte() }
            return AeadCipher.Sealed(iv, plaintext.map { (it.toInt() xor 0x5A).toByte() }.toByteArray() + tag(aad))
        }

        override fun decrypt(iv: ByteArray, ciphertext: ByteArray, aad: ByteArray): ByteArray {
            val body = ciphertext.copyOfRange(0, ciphertext.size - 16)
            if (!ciphertext.copyOfRange(ciphertext.size - 16, ciphertext.size).contentEquals(tag(aad))) {
                throw GeneralSecurityException("tag mismatch")
            }
            return body.map { (it.toInt() xor 0x5A).toByte() }.toByteArray()
        }

        override fun destroyKey() {
            destroyed = true
        }

        private fun tag(aad: ByteArray) = ByteArray(16) { if (it < aad.size) aad[it] else 0 }
    }

    private val cipher = FakeCipher()

    @Test
    fun framesVersionIvAndCiphertext() {
        val blob = SecretBlob.seal(cipher, "tunnel", "secret".toByteArray())
        assertEquals(1, blob[0].toInt())
        assertArrayEquals(ByteArray(12) { it.toByte() }, blob.copyOfRange(1, 13))
        assertEquals(1 + 12 + "secret".length + 16, blob.size)
    }

    @Test
    fun roundTrips() {
        val blob = SecretBlob.seal(cipher, "creds", "admin\u0000pw".toByteArray())
        assertArrayEquals("admin\u0000pw".toByteArray(), SecretBlob.open(cipher, "creds", blob))
    }

    @Test
    fun bindsTheEntryNameAsAad() {
        val blob = SecretBlob.seal(cipher, "tunnel", "x".toByteArray())
        assertArrayEquals("tunnel".toByteArray(), cipher.lastAad)
        assertThrows(GeneralSecurityException::class.java) { SecretBlob.open(cipher, "creds", blob) }
    }

    @Test
    fun rejectsUnknownVersionAndTruncation() {
        val blob = SecretBlob.seal(cipher, "tunnel", "x".toByteArray())
        val wrongVersion = blob.copyOf().also { it[0] = 2 }
        assertThrows(GeneralSecurityException::class.java) { SecretBlob.open(cipher, "tunnel", wrongVersion) }
        assertThrows(GeneralSecurityException::class.java) { SecretBlob.open(cipher, "tunnel", blob.copyOf(10)) }
        assertThrows(GeneralSecurityException::class.java) { SecretBlob.open(cipher, "tunnel", ByteArray(0)) }
    }
}
