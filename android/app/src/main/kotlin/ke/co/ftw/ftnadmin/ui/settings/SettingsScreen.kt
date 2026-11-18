package ke.co.ftw.ftnadmin.ui.settings

import androidx.activity.compose.LocalActivity
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.selection.selectable
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.RadioButton
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.unit.dp
import androidx.compose.ui.window.DialogProperties
import androidx.compose.ui.window.SecureFlagPolicy
import androidx.fragment.app.FragmentActivity
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import ke.co.ftw.ftnadmin.BuildConfig
import ke.co.ftw.ftnadmin.auth.AuthResult
import ke.co.ftw.ftnadmin.core.prefs.AppSettings
import ke.co.ftw.ftnadmin.core.prefs.DEFAULT_PUBLIC_URL
import ke.co.ftw.ftnadmin.ui.common.LocalContainer
import ke.co.ftw.ftnadmin.ui.common.TunnelImportPanel
import ke.co.ftw.ftnadmin.ui.common.TunnelSummaryBlock
import ke.co.ftw.ftnadmin.ui.common.importTunnel
import ke.co.ftw.ftnadmin.ui.common.isDeviceSecure
import ke.co.ftw.ftnadmin.ui.common.saveLogin
import ke.co.ftw.ftnadmin.ui.theme.supportsDynamicTheming
import kotlinx.coroutines.launch

private enum class Dialog { None, Import, Login, ForgetLogin, LockTimeout, AutoDisconnect, PublicUrl, Wipe, About }

private val lockTimeoutChoices = listOf(0, 1, 5, 15)
private val autoDisconnectChoices = listOf(0, 1, 5, 15, 30)

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun SettingsScreen(onBack: () -> Unit) {
    val container = LocalContainer.current
    val activity = LocalActivity.current as? FragmentActivity
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    val settings by container.settings.flow.collectAsStateWithLifecycle(AppSettings())
    val hasLogin by container.credentials.hasSaved.collectAsStateWithLifecycle()
    var dialog by remember { mutableStateOf(Dialog.None) }
    var message by remember { mutableStateOf<String?>(null) }

    fun update(transform: (AppSettings) -> AppSettings) {
        scope.launch { container.settings.update(transform) }
    }

    Scaffold(
        topBar = {
            TopAppBar(
                title = { Text("Settings") },
                navigationIcon = {
                    IconButton(onClick = onBack) { Icon(Icons.AutoMirrored.Filled.ArrowBack, contentDescription = "Back") }
                },
            )
        },
    ) { padding ->
        Column(Modifier.fillMaxSize().padding(padding).verticalScroll(rememberScrollState())) {
            message?.let {
                Text(it, Modifier.padding(16.dp), color = MaterialTheme.colorScheme.error)
            }
            Header("Tunnel")
            ActionRow("Re-import tunnel", "Replace the WireGuard config") { dialog = Dialog.Import }

            Header("Admin login")
            ActionRow(
                if (hasLogin) "Change saved password" else "Save a password",
                if (hasLogin) "A login is saved on this device" else "Otherwise you sign in when the server asks",
            ) {
                if (context.isDeviceSecure()) {
                    dialog = Dialog.Login
                } else {
                    message = "Set a screen lock on this phone before saving a password."
                }
            }
            if (hasLogin) ActionRow("Forget saved password", null) { dialog = Dialog.ForgetLogin }

            Header("Security")
            SwitchRow("App lock", "Fingerprint, face or screen lock", settings.biometricEnabled) { enable ->
                message = null
                if (!enable) {
                    update { it.copy(biometricEnabled = false) }
                } else if (activity != null && container.appLock.canAuthenticate(activity)) {
                    container.appLock.prompt(activity, "Turn on app lock") { r ->
                        when (r) {
                            AuthResult.Success -> update { it.copy(biometricEnabled = true) }
                            is AuthResult.Failure -> if (!r.canceled) message = r.message
                        }
                    }
                } else {
                    message = "This phone has no screen lock or biometric set up."
                }
            }
            if (settings.biometricEnabled) {
                ActionRow("Lock after", minutesLabel(settings.lockTimeoutMin, "in background")) { dialog = Dialog.LockTimeout }
            }
            SwitchRow("Hide content from screenshots", "Blocks screenshots and the recents preview", settings.flagSecure) { on ->
                update { it.copy(flagSecure = on) }
            }

            Header("Connection")
            SwitchRow("Auto-connect", "Connect when the app opens", settings.autoConnect) { on ->
                update { it.copy(autoConnect = on) }
            }
            ActionRow("Auto-disconnect", minutesLabel(settings.autoDisconnectMin, "in background")) { dialog = Dialog.AutoDisconnect }
            ActionRow("Public URL", settings.publicBaseUrl) { dialog = Dialog.PublicUrl }

            if (supportsDynamicTheming()) {
                Header("Appearance")
                SwitchRow("Use wallpaper colours (Android 12+)", "Match the system palette instead of the brand colours", settings.dynamicColor) { on ->
                    update { it.copy(dynamicColor = on) }
                }
            }

            Header("Other")
            ActionRow("About", "Version ${BuildConfig.VERSION_NAME}") { dialog = Dialog.About }
            ActionRow("Wipe app data", "Remove the tunnel, login and settings from this phone") { dialog = Dialog.Wipe }
        }
    }

    when (dialog) {
        Dialog.None -> Unit
        Dialog.Import -> ImportDialog(onClose = { dialog = Dialog.None })
        Dialog.Login -> LoginDialog(
            initialUser = container.credentials.savedUsername() ?: "admin",
            onClose = { dialog = Dialog.None },
        )
        Dialog.ForgetLogin -> ConfirmDialog(
            title = "Forget saved password?",
            body = "You will be asked to sign in again.",
            action = "Forget",
            onConfirm = {
                scope.launch(kotlinx.coroutines.Dispatchers.IO) { container.credentials.forget() }
                dialog = Dialog.None
            },
            onDismiss = { dialog = Dialog.None },
        )
        Dialog.LockTimeout -> ChoiceDialog(
            title = "Lock after",
            choices = lockTimeoutChoices,
            selected = settings.lockTimeoutMin,
            label = { minutesLabel(it, "in background") },
            onPick = { picked -> update { it.copy(lockTimeoutMin = picked) }; dialog = Dialog.None },
            onDismiss = { dialog = Dialog.None },
        )
        Dialog.AutoDisconnect -> ChoiceDialog(
            title = "Auto-disconnect",
            choices = autoDisconnectChoices,
            selected = settings.autoDisconnectMin,
            label = { minutesLabel(it, "in background") },
            onPick = { picked -> update { it.copy(autoDisconnectMin = picked) }; dialog = Dialog.None },
            onDismiss = { dialog = Dialog.None },
        )
        Dialog.PublicUrl -> PublicUrlDialog(
            current = settings.publicBaseUrl,
            onSave = { url -> update { it.copy(publicBaseUrl = url) }; dialog = Dialog.None },
            onDismiss = { dialog = Dialog.None },
        )
        Dialog.Wipe -> ConfirmDialog(
            title = "Wipe app data?",
            body = "This removes the tunnel, saved password and settings from this phone. " +
                "Afterwards run ftnbilling-admin-peer remove <name> on the server to revoke this device.",
            action = "Wipe",
            onConfirm = {
                dialog = Dialog.None
                scope.launch { container.wipe() }
            },
            onDismiss = { dialog = Dialog.None },
        )
        Dialog.About -> AboutDialog(onDismiss = { dialog = Dialog.None })
    }
}

private fun minutesLabel(minutes: Int, suffix: String) = when (minutes) {
    0 -> "Immediately $suffix"
    1 -> "After 1 minute $suffix"
    else -> "After $minutes minutes $suffix"
}

@Composable
private fun Header(text: String) {
    HorizontalDivider()
    Text(
        text,
        Modifier.padding(start = 16.dp, top = 16.dp, end = 16.dp, bottom = 4.dp),
        style = MaterialTheme.typography.labelLarge,
        color = MaterialTheme.colorScheme.primary,
    )
}

@Composable
private fun ActionRow(title: String, subtitle: String?, onClick: () -> Unit) {
    Column(Modifier.fillMaxWidth().clickable(onClick = onClick).padding(horizontal = 16.dp, vertical = 12.dp)) {
        Text(title, style = MaterialTheme.typography.bodyLarge)
        subtitle?.let { Text(it, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant) }
    }
}

@Composable
private fun SwitchRow(title: String, subtitle: String, checked: Boolean, onChange: (Boolean) -> Unit) {
    Row(
        Modifier.fillMaxWidth().clickable { onChange(!checked) }.padding(horizontal = 16.dp, vertical = 12.dp),
        verticalAlignment = Alignment.CenterVertically,
        horizontalArrangement = Arrangement.SpaceBetween,
    ) {
        Column(Modifier.weight(1f)) {
            Text(title, style = MaterialTheme.typography.bodyLarge)
            Text(subtitle, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
        Switch(checked = checked, onCheckedChange = onChange)
    }
}

@Composable
private fun ConfirmDialog(title: String, body: String, action: String, onConfirm: () -> Unit, onDismiss: () -> Unit) {
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text(title) },
        text = { Text(body) },
        confirmButton = { TextButton(onClick = onConfirm) { Text(action) } },
        dismissButton = { TextButton(onClick = onDismiss) { Text("Cancel") } },
    )
}

@Composable
private fun ChoiceDialog(
    title: String,
    choices: List<Int>,
    selected: Int,
    label: (Int) -> String,
    onPick: (Int) -> Unit,
    onDismiss: () -> Unit,
) {
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text(title) },
        text = {
            Column {
                choices.forEach { c ->
                    Row(
                        Modifier.fillMaxWidth().selectable(selected = c == selected, role = Role.RadioButton, onClick = { onPick(c) }),
                        verticalAlignment = Alignment.CenterVertically,
                    ) {
                        RadioButton(selected = c == selected, onClick = null)
                        Text(label(c), Modifier.padding(start = 12.dp, top = 12.dp, bottom = 12.dp))
                    }
                }
            }
        },
        confirmButton = { TextButton(onClick = onDismiss) { Text("Close") } },
    )
}

@Composable
private fun PublicUrlDialog(current: String, onSave: (String) -> Unit, onDismiss: () -> Unit) {
    var value by remember { mutableStateOf(current) }
    val trimmed = value.trim().trimEnd('/')
    val valid = trimmed.startsWith("https://") && trimmed.length > "https://".length && trimmed.none { it.isWhitespace() }
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text("Public URL") },
        text = {
            Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                Text("Used for the public health check. Must start with https://.")
                OutlinedTextField(
                    value,
                    { value = it },
                    singleLine = true,
                    isError = !valid,
                    keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Uri),
                )
                TextButton(onClick = { value = DEFAULT_PUBLIC_URL }) { Text("Reset to default") }
            }
        },
        confirmButton = { TextButton(onClick = { onSave(trimmed) }, enabled = valid) { Text("Save") } },
        dismissButton = { TextButton(onClick = onDismiss) { Text("Cancel") } },
    )
}

@Composable
private fun ImportDialog(onClose: () -> Unit) {
    val container = LocalContainer.current
    val scope = rememberCoroutineScope()
    val summary by container.tunnelRepository.summary.collectAsStateWithLifecycle()
    var busy by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf<String?>(null) }
    var imported by remember { mutableStateOf(false) }
    AlertDialog(
        onDismissRequest = onClose,
        properties = DialogProperties(securePolicy = SecureFlagPolicy.SecureOn),
        title = { Text("Re-import tunnel") },
        text = {
            Column(verticalArrangement = Arrangement.spacedBy(12.dp), modifier = Modifier.verticalScroll(rememberScrollState())) {
                Text("The new tunnel replaces the current one.")
                TunnelImportPanel(
                    busy = busy,
                    error = error,
                    onConfigText = { text ->
                        scope.launch {
                            busy = true
                            error = container.importTunnel(text)
                            imported = error == null
                            busy = false
                        }
                    },
                )
                if (imported) summary?.let { TunnelSummaryBlock(it) }
            }
        },
        confirmButton = { TextButton(onClick = onClose) { Text(if (imported) "Done" else "Close") } },
    )
}

@Composable
private fun LoginDialog(initialUser: String, onClose: () -> Unit) {
    val container = LocalContainer.current
    val scope = rememberCoroutineScope()
    var user by remember { mutableStateOf(initialUser) }
    var pass by remember { mutableStateOf("") }
    var error by remember { mutableStateOf<String?>(null) }
    var busy by remember { mutableStateOf(false) }
    AlertDialog(
        onDismissRequest = onClose,
        properties = DialogProperties(securePolicy = SecureFlagPolicy.SecureOn),
        title = { Text("Admin login") },
        text = {
            Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                OutlinedTextField(user, { user = it }, label = { Text("Username") }, singleLine = true)
                OutlinedTextField(
                    pass,
                    { pass = it },
                    label = { Text("Password") },
                    singleLine = true,
                    visualTransformation = PasswordVisualTransformation(),
                    keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Password),
                )
                error?.let { Text(it, color = MaterialTheme.colorScheme.error) }
            }
        },
        confirmButton = {
            TextButton(
                enabled = !busy,
                onClick = {
                    scope.launch {
                        busy = true
                        error = container.saveLogin(user, pass)
                        busy = false
                        if (error == null) onClose()
                    }
                },
            ) { Text("Save") }
        },
        dismissButton = { TextButton(onClick = onClose) { Text("Cancel") } },
    )
}

@Composable
private fun AboutDialog(onDismiss: () -> Unit) {
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text("FTN Admin") },
        text = {
            Column(Modifier.verticalScroll(rememberScrollState()), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                Text("Version ${BuildConfig.VERSION_NAME} (${BuildConfig.VERSION_CODE})")
                Text("WireGuard tunnel library ${BuildConfig.TUNNEL_LIB_VERSION}")
                Text("Licences", style = MaterialTheme.typography.titleSmall)
                Text("wireguard-android: Apache License 2.0")
                Text("wireguard-go: MIT License")
                Text("zxing-android-embedded and ZXing: Apache License 2.0")
                Text("AndroidX and Jetpack Compose: Apache License 2.0")
                Text("WireGuard is a registered trademark of Jason A. Donenfeld.", style = MaterialTheme.typography.bodySmall)
            }
        },
        confirmButton = { TextButton(onClick = onDismiss) { Text("Close") } },
    )
}
