package ke.co.ftw.ftnadmin.tunnel

import com.wireguard.android.backend.Tunnel
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow

/** The single tunnel this app manages. The backend reports state changes (including revocation) here. */
class FtnTunnel : Tunnel {
    private val _state = MutableStateFlow(Tunnel.State.DOWN)
    val state: StateFlow<Tunnel.State> = _state.asStateFlow()

    override fun getName(): String = NAME

    override fun onStateChange(newState: Tunnel.State) {
        _state.value = newState
    }

    companion object {
        const val NAME = "ftnadmin"
    }
}
