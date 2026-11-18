package ke.co.ftw.ftnadmin.ui.home

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import ke.co.ftw.ftnadmin.AppContainer
import ke.co.ftw.ftnadmin.net.Fetch
import ke.co.ftw.ftnadmin.net.FleetSummary
import ke.co.ftw.ftnadmin.net.ServerHealth
import ke.co.ftw.ftnadmin.tunnel.TunnelState
import kotlinx.coroutines.Job
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.launch

class HomeViewModel(private val container: AppContainer) : ViewModel() {
    private val _publicHealth = MutableStateFlow<Fetch<ServerHealth>>(Fetch.Loading)
    val publicHealth: StateFlow<Fetch<ServerHealth>> = _publicHealth.asStateFlow()
    private val _tunnelHealth = MutableStateFlow<Fetch<ServerHealth>>(Fetch.Loading)
    val tunnelHealth: StateFlow<Fetch<ServerHealth>> = _tunnelHealth.asStateFlow()
    private val _fleet = MutableStateFlow<Fetch<FleetSummary>>(Fetch.Loading)
    val fleet: StateFlow<Fetch<FleetSummary>> = _fleet.asStateFlow()

    private var tunnelJob: Job? = null

    init {
        refreshPublic()
        viewModelScope.launch {
            container.tunnel.state
                .map { it is TunnelState.Up }
                .distinctUntilChanged()
                .collect { up -> if (up) refreshTunnel() }
        }
    }

    fun refresh() {
        refreshPublic()
        if (container.tunnel.state.value is TunnelState.Up) refreshTunnel()
    }

    private fun refreshPublic() {
        viewModelScope.launch { _publicHealth.value = container.publicHealth.fetch() }
    }

    private fun refreshTunnel() {
        tunnelJob?.cancel()
        tunnelJob = viewModelScope.launch {
            _tunnelHealth.value = Fetch.Loading
            _fleet.value = Fetch.Loading
            _tunnelHealth.value = container.adminApi.health()
            _fleet.value = container.adminApi.summary()
        }
    }
}
