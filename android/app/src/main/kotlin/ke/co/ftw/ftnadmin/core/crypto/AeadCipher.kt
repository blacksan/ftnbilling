package ke.co.ftw.ftnadmin.core.crypto

/** Authenticated encryption with associated data. Kept as an interface so blob framing is unit-testable on the JVM. */
interface AeadCipher {
    class Sealed(val iv: ByteArray, val ciphertext: ByteArray)

    fun encrypt(plaintext: ByteArray, aad: ByteArray): Sealed

    fun decrypt(iv: ByteArray, ciphertext: ByteArray, aad: ByteArray): ByteArray

    /** Irrevocably deletes the key material; everything sealed with it becomes unreadable. */
    fun destroyKey()
}
