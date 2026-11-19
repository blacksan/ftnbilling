package ke.co.ftw.ftnadmin.ui

import ke.co.ftw.ftnadmin.ui.common.Tone
import ke.co.ftw.ftnadmin.ui.home.networkStatus
import org.junit.Assert.assertEquals
import org.junit.Test

class NetworkStatusTest {
    @Test fun noRoutersIsNeutralNotGreen() {
        assertEquals("No routers yet" to Tone.Neutral, networkStatus(0, 0))
    }

    @Test fun greenOnlyWhenAllOnline() {
        assertEquals(Tone.Good, networkStatus(3, 3).second)
        assertEquals(Tone.Bad, networkStatus(2, 3).second)
        assertEquals(Tone.Bad, networkStatus(0, 3).second)
    }
}
