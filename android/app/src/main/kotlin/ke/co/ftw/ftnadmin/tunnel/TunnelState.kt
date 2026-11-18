package ke.co.ftw.ftnadmin.tunnel

import android.net.Network
import kotlinx.coroutines.flow.StateFlow

data class TunnelStats(
    val rxBytes: Long,
    val txBytes: Long,
    /** Epoch millis of the latest handshake, 0 when there was none. */
    val lastHandshakeEpochMs: Long,
)

sealed interface TunnelState {
    data object NoConfig : TunnelState
    data object NeedsPermission : TunnelState
    data object Down : TunnelState
    data class Connecting(val stage: String) : TunnelState

    /** Handshake done and the server answered: the only state in which credentials may be released. */
    data class Up(val stats: TunnelStats) : TunnelState
    data class Error(val message: String) : TunnelState
}

/** The read-only view of the tunnel that networking and web code depend on. */
interface TunnelAccess {
    val state: StateFlow<TunnelState>

    /** The VPN network, or null when none is present. */
    fun vpnNetwork(): Network?
}

/** Asks the server over the tunnel whether it is answering. */
fun interface HealthProbe {
    suspend fun check(network: Network?): Boolean
}
