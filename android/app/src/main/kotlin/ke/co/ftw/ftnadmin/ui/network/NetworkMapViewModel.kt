package ke.co.ftw.ftnadmin.ui.network

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import ke.co.ftw.ftnadmin.data.networkmap.NetApartment
import ke.co.ftw.ftnadmin.data.networkmap.NetworkMap
import ke.co.ftw.ftnadmin.data.networkmap.NetworkMapRepository
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.common.apiResult
import ke.co.ftw.ftnadmin.ui.common.userMessage
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch

/** The three views of the Network screen. Map is the default; Live is the router-by-router view the screen had before. */
enum class NetworkMode(val label: String) { MapView("Map"), ListView("List"), Live("Live") }

data class NetworkMapUiState(
    val mode: NetworkMode = NetworkMode.MapView,
    val data: Loadable<NetworkMap> = Loadable.Loading,
    /** Id of the apartment whose sheet is open. */
    val selectedId: String? = null,
    val refreshing: Boolean = false,
    val message: String? = null,
) {
    private val map: NetworkMap? get() = (data as? Loadable.Ready)?.value

    val apartments: List<NetApartment> get() = map?.apartments.orEmpty()

    /** The apartment of the open sheet; null when none is open or it is gone after a refresh. */
    val selected: NetApartment? get() = apartments.firstOrNull { it.id == selectedId }

    val located: List<NetApartment> get() = splitByLocation(apartments).first

    val unlocated: List<NetApartment> get() = splitByLocation(apartments).second
}

/** The Network map: loads the apartments once, keeps the open sheet, refreshes on pull. */
class NetworkMapViewModel(private val repo: NetworkMapRepository) : ViewModel() {
    private val _state = MutableStateFlow(NetworkMapUiState())
    val state: StateFlow<NetworkMapUiState> = _state.asStateFlow()

    init {
        load(initial = true)
    }

    fun setMode(m: NetworkMode) = _state.update { it.copy(mode = m, selectedId = null) }

    fun select(id: String) = _state.update { it.copy(selectedId = id) }

    fun dismiss() = _state.update { it.copy(selectedId = null) }

    fun retry() = load(initial = true)

    fun refresh() {
        _state.update { it.copy(refreshing = true) }
        load(initial = false)
    }

    fun clearMessage() = _state.update { it.copy(message = null) }

    private fun load(initial: Boolean) {
        if (initial) _state.update { if (it.data is Loadable.Ready) it else it.copy(data = Loadable.Loading) }
        viewModelScope.launch {
            val r = apiResult { repo.map() }
            _state.update { cur ->
                r.fold(
                    { m -> cur.copy(data = Loadable.Ready(m), refreshing = false) },
                    { e ->
                        if (cur.data is Loadable.Ready) cur.copy(refreshing = false, message = e.userMessage())
                        else cur.copy(data = Loadable.Failed(e.userMessage()), refreshing = false)
                    },
                )
            }
        }
    }
}
