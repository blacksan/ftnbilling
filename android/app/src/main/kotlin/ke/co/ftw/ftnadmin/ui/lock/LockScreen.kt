package ke.co.ftw.ftnadmin.ui.lock

import androidx.activity.compose.LocalActivity
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.safeDrawingPadding
import androidx.compose.foundation.layout.size
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Lock
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.dp
import androidx.fragment.app.FragmentActivity
import ke.co.ftw.ftnadmin.auth.AuthResult
import ke.co.ftw.ftnadmin.ui.common.LocalContainer
import ke.co.ftw.ftnadmin.ui.common.SecureScreen
import kotlinx.coroutines.launch

private const val FAILURES_BEFORE_WIPE_OFFER = 3

@Composable
fun LockScreen() {
    SecureScreen()
    val container = LocalContainer.current
    val activity = LocalActivity.current as? FragmentActivity
    val scope = rememberCoroutineScope()
    var failures by rememberSaveable { mutableIntStateOf(0) }
    var message by rememberSaveable { mutableStateOf<String?>(null) }
    var confirmWipe by rememberSaveable { mutableStateOf(false) }

    fun prompt() {
        val host = activity ?: return
        if (!container.appLock.canAuthenticate(host)) {
            message = "No screen lock or biometric is available on this device, so the app cannot be unlocked."
            failures = FAILURES_BEFORE_WIPE_OFFER
            return
        }
        container.appLock.unlockWithPrompt(host) { result ->
            if (result is AuthResult.Failure) {
                failures++
                message = result.message
            }
        }
    }

    LaunchedEffect(Unit) { prompt() }

    Column(
        Modifier.fillMaxSize().safeDrawingPadding().padding(32.dp),
        verticalArrangement = Arrangement.Center,
        horizontalAlignment = Alignment.CenterHorizontally,
    ) {
        Icon(Icons.Default.Lock, contentDescription = null, modifier = Modifier.size(56.dp), tint = MaterialTheme.colorScheme.primary)
        Spacer(Modifier.height(16.dp))
        Text("FTN Admin is locked", style = MaterialTheme.typography.headlineSmall)
        message?.let {
            Spacer(Modifier.height(8.dp))
            Text(it, style = MaterialTheme.typography.bodyMedium, textAlign = TextAlign.Center, color = MaterialTheme.colorScheme.error)
        }
        Spacer(Modifier.height(24.dp))
        Button(onClick = ::prompt) { Text("Unlock") }
        if (failures >= FAILURES_BEFORE_WIPE_OFFER) {
            TextButton(onClick = { confirmWipe = true }) { Text("Wipe app data") }
        }
    }

    if (confirmWipe) {
        AlertDialog(
            onDismissRequest = { confirmWipe = false },
            title = { Text("Wipe app data?") },
            text = {
                Text(
                    "This removes the VPN tunnel, saved password and settings from this device. " +
                        "Afterwards run ftnbilling-admin-peer remove <name> on the server to revoke this device.",
                )
            },
            confirmButton = {
                TextButton(onClick = {
                    confirmWipe = false
                    scope.launch { container.wipe() }
                }) { Text("Wipe") }
            },
            dismissButton = { TextButton(onClick = { confirmWipe = false }) { Text("Cancel") } },
        )
    }
}
