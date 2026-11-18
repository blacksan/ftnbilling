package ke.co.ftw.ftnadmin.tunnel

import android.content.Context
import android.content.Intent
import android.net.ConnectivityManager
import android.net.Network
import android.net.NetworkCapabilities
import android.net.VpnService
import android.os.SystemClock
import android.util.Log
import com.wireguard.android.backend.Backend
import com.wireguard.android.backend.BackendException
import com.wireguard.android.backend.Tunnel
import com.wireguard.config.BadConfigException
import com.wireguard.config.Config
import java.io.ByteArrayInputStream
import java.io.IOException
import ke.co.ftw.ftnadmin.core.prefs.Settings
import kotlinx.coroutines.CoroutineDispatcher
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withContext

/**
 * Owns the tunnel lifecycle: connect (VPN consent is the caller's job, see [prepareIntent]),
 * the ready gate, stats polling and the idle auto-disconnect timer.
 *
 * Connect only while the activity is resumed and unlocked: the library starts the VpnService
 * with startService, which is only reliable from the foreground.
 */
class TunnelManager(
    context: Context,
    private val backend: Backend,
    private val repository: TunnelRepository,
    private val settings: Settings,
    private val probe: HealthProbe,
    private val scope: CoroutineScope,
    private val io: CoroutineDispatcher = Dispatchers.IO,
) : TunnelAccess {
    private val appContext = context.applicationContext
    private val connectivity = appContext.getSystemService(ConnectivityManager::class.java)
    private val tunnel = FtnTunnel()
    private val mutex = Mutex()
    private val _state = MutableStateFlow<TunnelState>(TunnelState.Down)
    override val state: StateFlow<TunnelState> = _state.asStateFlow()

    private var pollJob: Job? = null
    private var idleJob: Job? = null

    init {
        scope.launch {
            repository.summary.collect { summary ->
                val current = _state.value
                if (summary == null && current !is TunnelState.NoConfig) {
                    if (current is TunnelState.Up || current is TunnelState.Connecting) disconnect()
                    _state.value = TunnelState.NoConfig
                } else if (summary != null && current is TunnelState.NoConfig) {
                    _state.value = TunnelState.Down
                }
            }
        }
        scope.launch {
            // The backend reports revocation (another VPN took over, user turned it off in system settings).
            tunnel.state.collect { s ->
                if (s == Tunnel.State.DOWN && _state.value is TunnelState.Up) {
                    pollJob?.cancel()
                    _state.value = TunnelState.Down
                }
            }
        }
    }

    /** Non-null when the system VPN consent dialog must be shown first. */
    fun prepareIntent(): Intent? = VpnService.prepare(appContext)

    fun markNeedsPermission() {
        _state.value = TunnelState.NeedsPermission
    }

    override fun vpnNetwork(): Network? {
        @Suppress("DEPRECATION")
        val networks = connectivity.allNetworks
        return networks.firstOrNull {
            connectivity.getNetworkCapabilities(it)?.hasTransport(NetworkCapabilities.TRANSPORT_VPN) == true
        }
    }

    suspend fun connect() {
        mutex.withLock { connectLocked() }
    }

    private suspend fun connectLocked() {
        if (_state.value is TunnelState.Up) return
        val text = repository.configText()
        if (text == null) {
            _state.value = TunnelState.NoConfig
            return
        }
        if (prepareIntent() != null) {
            _state.value = TunnelState.NeedsPermission
            return
        }
        _state.value = TunnelState.Connecting("Starting tunnel")
        try {
            withContext(io) {
                val config = Config.parse(ByteArrayInputStream(text.toByteArray(Charsets.UTF_8)))
                backend.setState(tunnel, Tunnel.State.UP, config)
            }
        } catch (e: BackendException) {
            Log.w(TAG, "backend refused", e)
            fail(describe(e))
            return
        } catch (e: BadConfigException) {
            Log.w(TAG, "bad config", e)
            fail("The stored tunnel could not be loaded. Re-import it in Settings.")
            return
        } catch (e: IOException) {
            Log.w(TAG, "config io", e)
            fail("The stored tunnel could not be read. Re-import it in Settings.")
            return
        }
        _state.value = TunnelState.Connecting("Waiting for the server")
        when (val result = awaitReady()) {
            is Ready.Yes -> {
                _state.value = TunnelState.Up(result.stats)
                startPolling()
            }
            is Ready.No -> {
                // Fail closed: do not leave a half-working tunnel carrying admin traffic.
                runCatching { withContext(io) { backend.setState(tunnel, Tunnel.State.DOWN, null) } }
                fail(result.reason)
            }
        }
    }

    suspend fun disconnect() {
        mutex.withLock { disconnectLocked() }
    }

    private suspend fun disconnectLocked() {
        pollJob?.cancel()
        runCatching { withContext(io) { backend.setState(tunnel, Tunnel.State.DOWN, null) } }
            .onFailure { Log.w(TAG, "disconnect failed", it) }
        _state.value = if (repository.summary.value == null) TunnelState.NoConfig else TunnelState.Down
    }

    /** Process went to the background: drop the tunnel after the configured idle time. */
    fun onAppBackground() {
        idleJob?.cancel()
        idleJob = scope.launch {
            val minutes = settings.state.value?.autoDisconnectMin ?: DEFAULT_IDLE_MIN
            delay(minutes * 60_000L)
            if (_state.value is TunnelState.Up || _state.value is TunnelState.Connecting) disconnect()
        }
    }

    fun onAppForeground() {
        idleJob?.cancel()
        idleJob = null
    }

    private sealed interface Ready {
        data class Yes(val stats: TunnelStats) : Ready
        data class No(val reason: String) : Ready
    }

    private suspend fun awaitReady(): Ready {
        val deadline = SystemClock.elapsedRealtime() + READY_TIMEOUT_MS
        var handshake = false
        while (SystemClock.elapsedRealtime() < deadline) {
            val answered = probe.check(vpnNetwork())
            val stats = readStats()
            handshake = stats.lastHandshakeEpochMs > 0
            if (answered && handshake) return Ready.Yes(stats)
            delay(RETRY_DELAY_MS)
        }
        val port = repository.summary.value?.endpoint?.substringAfterLast(':')
        return Ready.No(
            if (!handshake) {
                "No handshake with the server. The device may have been removed on the server, " +
                    "or UDP${port?.let { " $it" }.orEmpty()} is blocked on this network."
            } else {
                "The tunnel is up but the admin server did not answer."
            },
        )
    }

    private suspend fun readStats(): TunnelStats = withContext(io) {
        try {
            val s = backend.getStatistics(tunnel)
            val handshake = s.peers().maxOfOrNull { s.peer(it)?.latestHandshakeEpochMillis() ?: 0L } ?: 0L
            TunnelStats(s.totalRx(), s.totalTx(), handshake)
        } catch (e: Exception) {
            Log.w(TAG, "statistics unavailable", e)
            TunnelStats(0, 0, 0)
        }
    }

    private fun startPolling() {
        pollJob?.cancel()
        pollJob = scope.launch {
            while (isActive && _state.value is TunnelState.Up) {
                delay(POLL_MS)
                val stats = readStats()
                if (_state.value is TunnelState.Up) _state.value = TunnelState.Up(stats)
            }
        }
    }

    private fun fail(message: String) {
        _state.value = TunnelState.Error(message)
    }

    private fun describe(e: BackendException): String = when (e.reason) {
        BackendException.Reason.VPN_NOT_AUTHORIZED ->
            "VPN permission is missing or another app is set as the always-on VPN. " +
                "Allow FTN Admin to set up a VPN, or turn off the other always-on VPN in system settings."
        BackendException.Reason.UNABLE_TO_START_VPN ->
            "Android could not start the VPN. Another VPN app may be active; turn it off and retry."
        BackendException.Reason.DNS_RESOLUTION_FAILURE ->
            "The server address could not be resolved. Check the network connection and retry."
        else -> "The tunnel could not start (${e.reason.name.lowercase().replace('_', ' ')})."
    }

    private companion object {
        const val TAG = "TunnelManager"
        const val DEFAULT_IDLE_MIN = 5
        const val POLL_MS = 2_000L
        const val RETRY_DELAY_MS = 500L
        const val READY_TIMEOUT_MS = 12_000L
    }
}
