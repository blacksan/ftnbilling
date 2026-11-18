package ke.co.ftw.ftnadmin.web

import org.junit.Test
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue

class UrlPolicyTest {
    private val policy = UrlPolicy("10.98.0.1", 8080)

    @Test
    fun allowsSameOrigin() {
        assertEquals(UrlDecision.Allow, policy.decide("http://10.98.0.1:8080/admin"))
        assertEquals(UrlDecision.Allow, policy.decide("http://10.98.0.1:8080/admin/sites/view?id=a"))
    }

    @Test
    fun interceptsLogout() {
        assertEquals(UrlDecision.Logout, policy.decide("http://10.98.0.1:8080/admin/logout"))
        assertEquals(UrlDecision.Logout, policy.decide("http://10.98.0.1:8080/admin/logout/"))
    }

    @Test
    fun opensExternalHttpsOutsideTheApp() {
        assertEquals(UrlDecision.OpenExternal, policy.decide("https://example.com/help"))
    }

    @Test
    fun blocksDangerousSchemes() {
        listOf(
            "javascript:alert(1)",
            "file:///data/data/ke.co.ftw.ftnadmin/files/secure/tunnel.bin",
            "intent://scan/#Intent;scheme=zxing;end",
            "content://com.example/x",
            "data:text/html,hi",
            "tel:123",
        ).forEach { assertEquals(it, UrlDecision.Block, policy.decide(it)) }
    }

    @Test
    fun blocksOtherPortsAndHostsOverHttp() {
        assertEquals(UrlDecision.Block, policy.decide("http://10.98.0.1:9090/admin"))
        assertEquals(UrlDecision.Block, policy.decide("http://10.98.0.1/admin"))
        assertEquals(UrlDecision.Block, policy.decide("http://10.98.0.2:8080/admin"))
        assertEquals(UrlDecision.Block, policy.decide("http://evil.example/admin"))
    }

    @Test
    fun blocksUserInfoTricks() {
        assertEquals(UrlDecision.Block, policy.decide("http://10.98.0.1:8080@evil.example/admin"))
        assertEquals(UrlDecision.Block, policy.decide("http://user:pw@10.98.0.1:8080/admin"))
    }

    @Test
    fun blocksGarbage() {
        assertEquals(UrlDecision.Block, policy.decide("not a url"))
        assertEquals(UrlDecision.Block, policy.decide(""))
    }

    @Test
    fun requestFilterAllowsOnlyAdminOrigin() {
        assertTrue(policy.allowsRequest("http://10.98.0.1:8080/static/app.css"))
        assertTrue(policy.allowsRequest("about:blank"))
        assertFalse(policy.allowsRequest("https://tracker.example/p.gif"))
        assertFalse(policy.allowsRequest("http://10.98.0.1:81/x"))
    }

    @Test
    fun buildsAdminUrls() {
        assertEquals("http://10.98.0.1:8080/admin", policy.adminUrl(null))
        assertEquals("http://10.98.0.1:8080/admin/alerts", policy.adminUrl("/admin/alerts"))
        assertEquals("http://10.98.0.1:8080/admin/sites/view?id=a%20b", policy.adminUrl("/admin/sites/view?id=a%20b"))
        assertNull(policy.adminUrl("/other"))
        assertNull(policy.adminUrl("//evil.example/admin"))
        assertNull(policy.adminUrl("http://evil.example/admin"))
    }
}
