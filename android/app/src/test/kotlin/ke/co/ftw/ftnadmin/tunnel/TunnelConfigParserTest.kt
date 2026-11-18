package ke.co.ftw.ftnadmin.tunnel

import java.util.Base64
import org.junit.Test
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertThrows
import org.junit.Assert.assertTrue

class TunnelConfigParserTest {
    private fun key(seed: Int) = Base64.getEncoder().encodeToString(ByteArray(32) { (it + seed).toByte() })

    private val privateKey = key(1)
    private val serverKey = key(2)

    /** Same layout as the output of deploy/ftnbilling-admin-peer. */
    private val valid = """
        [Interface]
        PrivateKey = $privateKey
        Address = 10.98.0.2/32

        [Peer]
        PublicKey = $serverKey
        Endpoint = isp.ftw.co.ke:51821
        AllowedIPs = 10.98.0.1/32
        PersistentKeepalive = 25
    """.trimIndent()

    private fun rejects(text: String): String =
        assertThrows(InvalidTunnelConfigException::class.java) { TunnelConfigParser.parse(text) }.message.orEmpty()

    @Test
    fun acceptsPeerScriptOutput() {
        val parsed = TunnelConfigParser.parse(valid)
        assertEquals("10.98.0.2/32", parsed.summary.address)
        assertEquals("isp.ftw.co.ke:51821", parsed.summary.endpoint)
        assertEquals(listOf("10.98.0.1/32"), parsed.summary.allowedIps)
        assertTrue(parsed.sanitizedConfig.contains("PersistentKeepalive = 25"))
    }

    @Test
    fun summaryNeverContainsKeys() {
        val parsed = TunnelConfigParser.parse(valid)
        assertFalse(parsed.summary.toString().contains(privateKey))
        assertFalse(parsed.toString().contains(privateKey))
    }

    @Test
    fun stripsDnsLines() {
        val parsed = TunnelConfigParser.parse(valid.replace("Address = 10.98.0.2/32", "Address = 10.98.0.2/32\nDNS = 1.1.1.1, 8.8.8.8"))
        assertFalse(parsed.sanitizedConfig.contains("DNS", ignoreCase = true))
    }

    @Test
    fun toleratesCommentsCaseAndCrlf() {
        val text = valid.replace("\n", "\r\n").replace("[Peer]", "# server\n[peer]").replace("Endpoint", "endpoint")
        TunnelConfigParser.parse(text)
    }

    @Test
    fun acceptsWholeAdminNetworkRoute() {
        TunnelConfigParser.parse(valid.replace("10.98.0.1/32", "10.98.0.0/24"))
    }

    @Test
    fun rejectsDefaultRoute() {
        assertTrue(rejects(valid.replace("10.98.0.1/32", "0.0.0.0/0")).contains("outside the admin network"))
    }

    @Test
    fun rejectsRouteOutsideAdminNetwork() {
        rejects(valid.replace("10.98.0.1/32", "10.98.0.1/32, 192.168.1.0/24"))
        rejects(valid.replace("10.98.0.1/32", "10.98.0.1/32, 10.98.0.0/16"))
    }

    @Test
    fun requiresServerAddressInAllowedIps() {
        assertTrue(rejects(valid.replace("10.98.0.1/32", "10.98.0.9/32")).contains("10.98.0.1"))
    }

    @Test
    fun rejectsTwoPeers() {
        val second = "\n[Peer]\nPublicKey = ${key(3)}\nAllowedIPs = 10.98.0.1/32\nEndpoint = a.example:1"
        assertTrue(rejects(valid + second).contains("exactly one"))
    }

    @Test
    fun rejectsMissingOrBadKeys() {
        rejects(valid.replace("PrivateKey = $privateKey\n", ""))
        rejects(valid.replace("PublicKey = $serverKey\n", ""))
        rejects(valid.replace(privateKey, "not-a-key"))
    }

    @Test
    fun rejectsAddressOutsideAdminNetwork() {
        rejects(valid.replace("10.98.0.2/32", "192.168.5.2/32"))
    }

    @Test
    fun rejectsCommandSettings() {
        assertTrue(rejects(valid.replace("[Peer]", "PostUp = reboot\n[Peer]")).contains("PostUp"))
    }

    @Test
    fun rejectsBadEndpoint() {
        rejects(valid.replace("isp.ftw.co.ke:51821", "isp.ftw.co.ke"))
        rejects(valid.replace("isp.ftw.co.ke:51821", "isp.ftw.co.ke:99999"))
    }

    @Test
    fun rejectsGarbage() {
        rejects("")
        rejects("hello world")
        rejects("<html>not a config</html>")
        rejects("[Interface]\nnonsense")
    }
}
