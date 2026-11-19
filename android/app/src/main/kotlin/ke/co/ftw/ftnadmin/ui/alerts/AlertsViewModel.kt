package ke.co.ftw.ftnadmin.ui.alerts

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import ke.co.ftw.ftnadmin.data.alerts.AlertsRepository
import ke.co.ftw.ftnadmin.domain.Alerts
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.common.apiResult
import ke.co.ftw.ftnadmin.ui.common.userMessage
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch

data class AlertsUiState(
    val alerts: Loadable<Alerts> = Loadable.Loading,
    val refreshing: Boolean = false,
    /** Building whose mute dialog is open. */
    val muteSite: Pair<String, String>? = null,
    val busy: Boolean = false,
    val message: String? = null,
)

/** Mute choices in minutes; 0 means unmute. */
val MUTE_CHOICES = listOf(30 to "30 minutes", 120 to "2 hours", 480 to "8 hours", 1440 to "24 hours")

class AlertsViewModel(private val repo: AlertsRepository) : ViewModel() {
    private val _state = MutableStateFlow(AlertsUiState())
    val state: StateFlow<AlertsUiState> = _state.asStateFlow()

    init {
        load(initial = true)
    }

    fun retry() = load(initial = true)

    fun refresh() = load(initial = false)

    private fun load(initial: Boolean) {
        _state.update { if (initial) it.copy(alerts = Loadable.Loading) else it.copy(refreshing = true) }
        viewModelScope.launch {
            val r = apiResult { repo.alerts() }
            _state.update { cur ->
                r.fold(
                    { a -> cur.copy(alerts = Loadable.Ready(a), refreshing = false) },
                    { e ->
                        if (cur.alerts is Loadable.Ready) cur.copy(refreshing = false, message = e.userMessage())
                        else cur.copy(alerts = Loadable.Failed(e.userMessage()), refreshing = false)
                    },
                )
            }
        }
    }

    fun openMute(siteId: String, siteName: String) = _state.update { it.copy(muteSite = siteId to siteName) }

    fun dismissMute() = _state.update { it.copy(muteSite = null) }

    fun clearMessage() = _state.update { it.copy(message = null) }

    /** [minutes] 0 unmutes. */
    fun mute(siteId: String, minutes: Int) {
        _state.update { it.copy(busy = true, message = null) }
        viewModelScope.launch {
            val r = apiResult { repo.mute(siteId, minutes) }
            _state.update {
                it.copy(
                    busy = false,
                    muteSite = null,
                    message = r.fold({ if (minutes == 0) "Alerts unmuted" else "Alerts muted" }, { e -> e.userMessage() }),
                )
            }
            if (r.isSuccess) load(initial = false)
        }
    }
}
