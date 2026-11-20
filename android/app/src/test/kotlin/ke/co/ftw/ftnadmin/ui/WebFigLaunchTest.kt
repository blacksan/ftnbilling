package ke.co.ftw.ftnadmin.ui

import ke.co.ftw.ftnadmin.core.network.ApiJson
import ke.co.ftw.ftnadmin.domain.Site
import ke.co.ftw.ftnadmin.ui.routers.webfigLaunchUrl
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

class WebFigLaunchTest {
    private val url = "https://isp.ftw.co.ke/admin/routers/webfig?id=s1a2b3c"

    @Test fun ownersGetTheServersAdminPage() {
        assertEquals(url, webfigLaunchUrl(url, isOwner = true))
    }

    @Test fun operatorsAndViewersNeverGetTheButton() {
        assertNull(webfigLaunchUrl(url, isOwner = false))
    }

    @Test fun noAddressMeansNoButton() {
        assertNull(webfigLaunchUrl("", isOwner = true))
    }

    @Test fun onlyHttpsIsEverOpened() {
        assertNull(webfigLaunchUrl("http://isp.ftw.co.ke/admin/routers/webfig?id=s1", isOwner = true))
        assertNull(webfigLaunchUrl("intent://x#Intent;end", isOwner = true))
        assertNull(webfigLaunchUrl("javascript:alert(1)", isOwner = true))
    }

    @Test fun siteJsonCarriesTheAddressAndDefaultsToNone() {
        val with = ApiJson.decodeFromString(Site.serializer(), """{"router_id":"r1","webfig_url":"$url"}""")
        assertEquals(url, with.webfigUrl)
        assertEquals("", ApiJson.decodeFromString(Site.serializer(), """{"router_id":"r1"}""").webfigUrl)
    }
}
