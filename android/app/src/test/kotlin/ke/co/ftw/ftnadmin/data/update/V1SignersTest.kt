package ke.co.ftw.ftnadmin.data.update

import java.io.File
import java.io.FileInputStream
import java.io.FileOutputStream
import java.security.KeyStore
import java.security.MessageDigest
import java.util.zip.ZipEntry
import java.util.zip.ZipFile
import java.util.zip.ZipOutputStream
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Assume.assumeTrue
import org.junit.Before
import org.junit.Test

class V1SignersTest {
    private lateinit var dir: File
    private val bin = File(System.getProperty("java.home"), "bin")

    @Before fun setUp() {
        dir = File.createTempFile("ftn-v1", "").also {
            it.delete()
            it.mkdirs()
        }
    }

    @After fun tearDown() {
        dir.deleteRecursively()
    }

    private fun run(vararg cmd: String) {
        val p = ProcessBuilder(*cmd).redirectErrorStream(true).start()
        val out = p.inputStream.bufferedReader().readText()
        assertEquals("command failed: ${cmd.first()} $out", 0, p.waitFor())
    }

    private fun tool(name: String): String {
        val f = File(bin, name)
        assumeTrue("$name not available in this JDK", f.exists())
        return f.path
    }

    private fun zip(target: File, entries: Map<String, String>) {
        ZipOutputStream(FileOutputStream(target)).use { z ->
            entries.forEach { (n, c) ->
                z.putNextEntry(ZipEntry(n))
                z.write(c.toByteArray())
                z.closeEntry()
            }
        }
    }

    private fun signed(): Pair<File, String> {
        val ks = File(dir, "k.p12")
        run(tool("keytool"), "-genkeypair", "-keystore", ks.path, "-storetype", "PKCS12", "-storepass", "secret1", "-keypass", "secret1",
            "-alias", "t", "-keyalg", "RSA", "-keysize", "2048", "-dname", "CN=test", "-validity", "30")
        val jar = File(dir, "a.apk")
        zip(jar, mapOf("classes.dex" to "dex-bytes", "res/a.txt" to "hello"))
        run(tool("jarsigner"), "-keystore", ks.path, "-storepass", "secret1", "-keypass", "secret1", jar.path, "t")
        val store = KeyStore.getInstance("PKCS12").also { k -> FileInputStream(ks).use { k.load(it, "secret1".toCharArray()) } }
        val hash = MessageDigest.getInstance("SHA-256").digest(store.getCertificate("t").encoded).joinToString("") { "%02x".format(it) }
        return jar to hash
    }

    @Test fun readsTheSignerCertificateHashOfAV1SignedApk() {
        val (jar, hash) = signed()
        assertEquals(setOf(hash), V1Signers.read(jar))
    }

    @Test fun unsignedApkHasNoSigners() {
        val jar = File(dir, "u.apk")
        zip(jar, mapOf("classes.dex" to "dex-bytes"))
        assertTrue(V1Signers.read(jar).isEmpty())
    }

    @Test fun tamperedEntryIsNotTrusted() {
        val (jar, _) = signed()
        val bad = File(dir, "bad.apk")
        ZipFile(jar).use { zf ->
            ZipOutputStream(FileOutputStream(bad)).use { out ->
                for (e in zf.entries()) {
                    out.putNextEntry(ZipEntry(e.name))
                    val bytes = zf.getInputStream(e).readBytes()
                    out.write(if (e.name == "classes.dex") "evil-bytes".toByteArray() else bytes)
                    out.closeEntry()
                }
            }
        }
        assertTrue(V1Signers.read(bad).isEmpty())
    }

    @Test fun missingFileHasNoSigners() {
        assertTrue(V1Signers.read(File(dir, "nope.apk")).isEmpty())
    }
}
