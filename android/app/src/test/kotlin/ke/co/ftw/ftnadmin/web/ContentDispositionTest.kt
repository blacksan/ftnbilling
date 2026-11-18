package ke.co.ftw.ftnadmin.web

import org.junit.Test
import org.junit.Assert.assertEquals

class ContentDispositionTest {
    @Test
    fun readsQuotedAndBareNames() {
        assertEquals("payments.csv", ContentDisposition.fileName("attachment; filename=\"payments.csv\""))
        assertEquals("payments.csv", ContentDisposition.fileName("attachment; filename=payments.csv"))
    }

    @Test
    fun prefersEncodedName() {
        assertEquals("a b.csv", ContentDisposition.fileName("attachment; filename=\"x.csv\"; filename*=UTF-8''a%20b.csv"))
    }

    @Test
    fun stripsPathsAndFallsBack() {
        assertEquals("passwd", ContentDisposition.fileName("attachment; filename=\"../../etc/passwd\""))
        assertEquals("export.csv", ContentDisposition.fileName(null))
        assertEquals("export.csv", ContentDisposition.fileName("attachment"))
    }
}
