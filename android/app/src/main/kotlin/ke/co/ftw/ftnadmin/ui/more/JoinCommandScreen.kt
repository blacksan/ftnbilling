package ke.co.ftw.ftnadmin.ui.more

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Button
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import androidx.lifecycle.ViewModel
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewModelScope
import androidx.lifecycle.viewmodel.compose.viewModel
import ke.co.ftw.ftnadmin.core.network.UserInfo
import ke.co.ftw.ftnadmin.data.sites.JoinCommand
import ke.co.ftw.ftnadmin.data.sites.SiteSetupRepository
import ke.co.ftw.ftnadmin.ui.common.AppTopBar
import ke.co.ftw.ftnadmin.ui.common.LocalContainer
import ke.co.ftw.ftnadmin.ui.common.SectionCard
import ke.co.ftw.ftnadmin.ui.common.apiResult
import ke.co.ftw.ftnadmin.ui.common.userMessage
import ke.co.ftw.ftnadmin.ui.common.vmFactory
import ke.co.ftw.ftnadmin.ui.routers.setup.CommandBox
import ke.co.ftw.ftnadmin.ui.routers.setup.CopyButton
import ke.co.ftw.ftnadmin.ui.routers.setup.expiryText
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch

data class JoinUiState(
    val busy: Boolean = false,
    /** In memory only: never persisted, never logged, dropped when the screen closes. */
    val command: JoinCommand? = null,
    val error: String? = null,
)

/** One-time join command for an existing router (owner). Never cached. */
class JoinCommandViewModel(private val routerId: String, private val repo: SiteSetupRepository) : ViewModel() {
    private val _state = MutableStateFlow(JoinUiState())
    val state: StateFlow<JoinUiState> = _state.asStateFlow()

    fun request() {
        if (_state.value.busy) return
        _state.value = JoinUiState(busy = true)
        viewModelScope.launch {
            val r = apiResult { repo.joinCommand(routerId) }
            _state.value = r.fold({ c -> JoinUiState(command = c) }, { e -> JoinUiState(error = e.userMessage()) })
        }
    }

    fun hide() {
        _state.update { JoinUiState() }
    }

    override fun onCleared() {
        // The command is a one-time secret: nothing keeps a copy once the screen is gone.
        _state.value = JoinUiState()
    }
}

@Composable
fun JoinCommandScreen(routerId: String, user: UserInfo, onBack: () -> Unit) {
    val container = LocalContainer.current
    val vm: JoinCommandViewModel = viewModel(key = "join-$routerId", factory = vmFactory { JoinCommandViewModel(routerId, container.siteSetup) })
    val ui by vm.state.collectAsStateWithLifecycle()
    Scaffold(
        containerColor = MaterialTheme.colorScheme.background,
        topBar = { AppTopBar("Join command", onBack = onBack) },
    ) { inner ->
        Column(
            Modifier.padding(inner).fillMaxSize().verticalScroll(rememberScrollState()).padding(16.dp),
            verticalArrangement = Arrangement.spacedBy(12.dp),
        ) {
            if (!user.isOwner) {
                Text("Only an owner can get a join command.", color = MaterialTheme.colorScheme.onSurfaceVariant)
            } else {
                SectionCard("Join command") {
                    val cmd = ui.command
                    if (cmd == null) {
                        Text(
                            "Creates a one-time command to paste into the router terminal. It is shown here only, never saved on this phone.",
                            style = MaterialTheme.typography.bodySmall,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                        ui.error?.let { Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall) }
                        Button(onClick = vm::request, enabled = !ui.busy, modifier = Modifier.fillMaxWidth()) {
                            Text(if (ui.busy) "Getting command..." else "Get join command", maxLines = 1)
                        }
                    } else {
                        JoinCommandBlock(cmd)
                        TextButton(onClick = vm::hide) { Text("Hide command") }
                    }
                }
            }
        }
    }
}

/** The one-time command with copy buttons. Used by Join command and by Add router. */
@Composable
fun JoinCommandBlock(cmd: JoinCommand) {
    Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
        Text(expiryText(cmd.expiresAt), style = MaterialTheme.typography.labelLarge, color = MaterialTheme.colorScheme.primary)
        CommandBox(cmd.command)
        CopyButton("Copy command", cmd.command)
        if (cmd.commandNocheck.isNotBlank()) {
            Text(
                "Skips the device-mode check. Use only if the router is already set up for scripts.",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            CopyButton("Copy command without check", cmd.commandNocheck)
        }
    }
}
