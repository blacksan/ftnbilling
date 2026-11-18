package ke.co.ftw.ftnadmin.ui.onboarding

import android.app.Activity
import androidx.activity.compose.LocalActivity
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.safeDrawingPadding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.unit.dp
import androidx.fragment.app.FragmentActivity
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import androidx.lifecycle.viewmodel.initializer
import androidx.lifecycle.viewmodel.viewModelFactory
import ke.co.ftw.ftnadmin.auth.AuthResult
import ke.co.ftw.ftnadmin.ui.common.LocalContainer
import ke.co.ftw.ftnadmin.ui.common.SecureScreen
import ke.co.ftw.ftnadmin.ui.common.TunnelImportPanel
import ke.co.ftw.ftnadmin.ui.common.TunnelSummaryBlock
import ke.co.ftw.ftnadmin.ui.common.isDeviceSecure
import androidx.compose.ui.platform.LocalContext

@Composable
fun OnboardingScreen() {
    val container = LocalContainer.current
    val vm: OnboardingViewModel = viewModel(factory = viewModelFactory { initializer { OnboardingViewModel(container) } })
    val summary by container.tunnelRepository.summary.collectAsStateWithLifecycle()
    val wiped by container.wiped.collectAsStateWithLifecycle()

    Column(
        Modifier.fillMaxSize().safeDrawingPadding().verticalScroll(rememberScrollState()).padding(24.dp),
        verticalArrangement = Arrangement.spacedBy(16.dp),
    ) {
        Text("FTN Admin", style = MaterialTheme.typography.headlineMedium)
        if (vm.busy) LinearProgressIndicator(Modifier.fillMaxWidth())
        when (vm.step) {
            OnboardingStep.Welcome -> {
                Text(
                    "Manage your ISP billing over a private WireGuard tunnel. Nothing is exposed to the internet, " +
                        "and this app only ever routes traffic to the admin server.",
                    style = MaterialTheme.typography.bodyLarge,
                )
                if (wiped) {
                    Card(colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.secondaryContainer)) {
                        Text(
                            "App data was wiped. To revoke this phone on the server, run:\nftnbilling-admin-peer remove <name>",
                            Modifier.padding(16.dp),
                            style = MaterialTheme.typography.bodyMedium,
                        )
                    }
                }
                Text("You need a device config from the server: run ftnbilling-admin-peer add <name> and keep the QR code ready.")
                Button(onClick = { vm.goTo(OnboardingStep.Import) }, modifier = Modifier.fillMaxWidth()) { Text("Get started") }
            }

            OnboardingStep.Import -> {
                Text("Import the tunnel", style = MaterialTheme.typography.titleLarge)
                Text("Scan the QR code printed by ftnbilling-admin-peer, or pick or paste the .conf file.")
                TunnelImportPanel(busy = vm.busy, error = vm.error, onConfigText = vm::importConfig)
                summary?.let {
                    Card {
                        Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                            Text("Tunnel ready", style = MaterialTheme.typography.titleMedium)
                            TunnelSummaryBlock(it)
                        }
                    }
                    Button(onClick = { vm.goTo(OnboardingStep.Vpn) }, modifier = Modifier.fillMaxWidth()) { Text("Continue") }
                }
            }

            OnboardingStep.Vpn -> VpnStep(onDone = { vm.goTo(OnboardingStep.Login) })

            OnboardingStep.Login -> LoginStep(
                busy = vm.busy,
                error = vm.error,
                onSave = { user, pass -> vm.saveLogin(user, pass) { vm.goTo(OnboardingStep.Lock) } },
                onSkip = { vm.goTo(OnboardingStep.Lock) },
                onError = vm::reportError,
            )

            OnboardingStep.Lock -> LockStep(
                enabled = vm.lockEnabled,
                error = vm.error,
                onEnabled = vm::enableLock,
                onError = vm::reportError,
                onFinish = vm::finish,
            )
        }
    }
}

@Composable
private fun VpnStep(onDone: () -> Unit) {
    val container = LocalContainer.current
    var granted by remember { mutableStateOf(container.tunnel.prepareIntent() == null) }
    val launcher = rememberLauncherForActivityResult(ActivityResultContracts.StartActivityForResult()) {
        granted = it.resultCode == Activity.RESULT_OK
    }
    Text("Allow the VPN", style = MaterialTheme.typography.titleLarge)
    Text(
        "Android asks once for permission to create a VPN. FTN Admin uses it only to reach the admin server; " +
            "the rest of your traffic is not touched.",
    )
    if (granted) {
        Text("VPN permission granted.", color = MaterialTheme.colorScheme.primary)
        Button(onClick = onDone, modifier = Modifier.fillMaxWidth()) { Text("Continue") }
    } else {
        Button(
            onClick = {
                val intent = container.tunnel.prepareIntent()
                if (intent == null) granted = true else launcher.launch(intent)
            },
            modifier = Modifier.fillMaxWidth(),
        ) { Text("Allow VPN") }
        TextButton(onClick = onDone, modifier = Modifier.fillMaxWidth()) { Text("Later") }
    }
}

@Composable
private fun LoginStep(
    busy: Boolean,
    error: String?,
    onSave: (String, String) -> Unit,
    onSkip: () -> Unit,
    onError: (String?) -> Unit,
) {
    SecureScreen()
    val context = LocalContext.current
    var user by rememberSaveable { mutableStateOf("admin") }
    var pass by remember { mutableStateOf("") }
    Text("Admin login", style = MaterialTheme.typography.titleLarge)
    Text(
        "Optional. Saved on this device, encrypted with a hardware-backed key, and only ever sent through the tunnel. " +
            "You can skip this and sign in when the server asks.",
    )
    OutlinedTextField(user, { user = it }, label = { Text("Username") }, singleLine = true, modifier = Modifier.fillMaxWidth())
    OutlinedTextField(
        pass,
        { pass = it },
        label = { Text("Password") },
        singleLine = true,
        visualTransformation = PasswordVisualTransformation(),
        keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Password),
        modifier = Modifier.fillMaxWidth(),
    )
    error?.let { Text(it, color = MaterialTheme.colorScheme.error) }
    Button(
        onClick = {
            if (!context.isDeviceSecure()) {
                onError("Set a screen lock (PIN, pattern or password) on this phone before saving a password.")
            } else {
                onSave(user, pass)
            }
        },
        enabled = !busy,
        modifier = Modifier.fillMaxWidth(),
    ) { Text("Save and continue") }
    OutlinedButton(onClick = onSkip, modifier = Modifier.fillMaxWidth()) { Text("Skip") }
}

@Composable
private fun LockStep(
    enabled: Boolean,
    error: String?,
    onEnabled: () -> Unit,
    onError: (String?) -> Unit,
    onFinish: () -> Unit,
) {
    val container = LocalContainer.current
    val activity = LocalActivity.current as? FragmentActivity
    Text("App lock", style = MaterialTheme.typography.titleLarge)
    Text("Ask for your fingerprint, face or screen lock when the app is opened.")
    error?.let { Text(it, color = MaterialTheme.colorScheme.error) }
    if (enabled) {
        Text("App lock is on.", color = MaterialTheme.colorScheme.primary)
    } else {
        Button(
            onClick = {
                val host = activity ?: return@Button
                if (!container.appLock.canAuthenticate(host)) {
                    onError("This phone has no screen lock or biometric set up. Add one in system settings first.")
                } else {
                    container.appLock.prompt(host, "Turn on app lock") { r ->
                        when (r) {
                            AuthResult.Success -> onEnabled()
                            is AuthResult.Failure -> onError(if (r.canceled) null else r.message)
                        }
                    }
                }
            },
            modifier = Modifier.fillMaxWidth(),
        ) { Text("Turn on app lock") }
    }
    Spacer(Modifier.height(8.dp))
    Button(onClick = onFinish, modifier = Modifier.fillMaxWidth()) { Text(if (enabled) "Finish" else "Finish without lock") }
}
