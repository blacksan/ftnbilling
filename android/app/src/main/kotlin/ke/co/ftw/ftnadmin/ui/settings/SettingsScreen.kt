package ke.co.ftw.ftnadmin.ui.settings

import androidx.activity.compose.LocalActivity
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Button
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.RadioButton
import androidx.compose.material3.Scaffold
import androidx.compose.material3.SegmentedButton
import androidx.compose.material3.SegmentedButtonDefaults
import androidx.compose.material3.SingleChoiceSegmentedButtonRow
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
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.fragment.app.FragmentActivity
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import ke.co.ftw.ftnadmin.BuildConfig
import ke.co.ftw.ftnadmin.auth.AuthResult
import ke.co.ftw.ftnadmin.core.network.AuthState
import ke.co.ftw.ftnadmin.core.prefs.AppSettings
import ke.co.ftw.ftnadmin.core.prefs.ThemeMode
import ke.co.ftw.ftnadmin.domain.DeviceSession
import ke.co.ftw.ftnadmin.ui.common.ErrorState
import ke.co.ftw.ftnadmin.ui.common.LoadingSpinner
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.common.LocalContainer
import ke.co.ftw.ftnadmin.ui.common.StatusChip
import ke.co.ftw.ftnadmin.ui.common.Tone
import ke.co.ftw.ftnadmin.ui.common.formatAge
import ke.co.ftw.ftnadmin.ui.common.formatDateTime
import ke.co.ftw.ftnadmin.ui.common.vmFactory
import ke.co.ftw.ftnadmin.ui.theme.supportsDynamicTheming
import ke.co.ftw.ftnadmin.ui.update.UpdateCard
import kotlinx.coroutines.launch

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun SettingsScreen(onBack: () -> Unit) {
    val container = LocalContainer.current
    val vm: SettingsViewModel = viewModel(factory = vmFactory { SettingsViewModel(container.auth) })
    val ui by vm.state.collectAsStateWithLifecycle()
    val settings by container.settings.state.collectAsStateWithLifecycle()
    val auth by container.sessions.state.collectAsStateWithLifecycle()
    val activity = LocalActivity.current as? FragmentActivity
    val scope = rememberCoroutineScope()
    var message by remember { mutableStateOf<String?>(null) }
    var confirmRevoke by remember { mutableStateOf<DeviceSession?>(null) }
    var confirmLogout by remember { mutableStateOf(false) }
    var lockDialog by remember { mutableStateOf(false) }

    fun update(t: (AppSettings) -> AppSettings) {
        scope.launch { container.settings.update(t) }
    }

    Scaffold(
        containerColor = MaterialTheme.colorScheme.background,
        topBar = {
            TopAppBar(
                title = { Text("Settings") },
                navigationIcon = {
                    IconButton(onClick = onBack) { Icon(Icons.AutoMirrored.Filled.ArrowBack, contentDescription = "Back") }
                },
            )
        },
    ) { inner ->
        val s = settings
        Column(Modifier.fillMaxSize().padding(inner).verticalScroll(rememberScrollState())) {
            Header("Account")
            (auth as? AuthState.SignedIn)?.let {
                Row2("Signed in as", "${it.user.username} (${it.user.role})")
            }
            Row2("Server", BuildConfig.API_BASE)
            Column(Modifier.padding(horizontal = 16.dp, vertical = 8.dp)) {
                OutlinedButton(onClick = { confirmLogout = true }) { Text("Sign out") }
            }

            Header("Signed-in devices")
            when (val d = ui.devices) {
                Loadable.Loading -> Column(Modifier.fillMaxWidth().padding(16.dp), horizontalAlignment = Alignment.CenterHorizontally) { LoadingSpinner() }
                is Loadable.Failed -> Column(Modifier.height2()) { ErrorState(d.message, vm::loadDevices) }
                is Loadable.Ready -> d.value.forEach { dev -> DeviceRow(dev, revoking = ui.revoking == dev.id) { confirmRevoke = dev } }
            }
            (ui.message ?: message)?.let {
                Text(it, color = MaterialTheme.colorScheme.error, modifier = Modifier.padding(16.dp), style = MaterialTheme.typography.bodyMedium)
            }
            HorizontalDivider()

            if (s != null) {
                Header("Security")
                SwitchRow("App lock", "Fingerprint, face or screen lock", s.biometricEnabled) { enable ->
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
                if (s.biometricEnabled) {
                    ActionRow("Lock after", "${s.lockTimeoutMin} min in background") { lockDialog = true }
                }
                SwitchRow("Hide content from screenshots", "Blocks screenshots and the recents preview", s.flagSecure) { on ->
                    update { it.copy(flagSecure = on) }
                }
                Header("Appearance")
                Text("Theme", style = MaterialTheme.typography.bodyLarge, color = MaterialTheme.colorScheme.onSurface, modifier = Modifier.padding(horizontal = 16.dp, vertical = 4.dp))
                SingleChoiceSegmentedButtonRow(Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 4.dp)) {
                    ThemeMode.entries.forEachIndexed { index, m ->
                        SegmentedButton(
                            selected = s.themeMode == m,
                            onClick = { update { it.copy(themeMode = m) } },
                            shape = SegmentedButtonDefaults.itemShape(index, ThemeMode.entries.size),
                            label = { Text(m.label, maxLines = 1, softWrap = false) },
                        )
                    }
                }
                Text(
                    "System follows the phone's light or dark setting.",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.padding(horizontal = 16.dp),
                )
                if (supportsDynamicTheming()) {
                    SwitchRow("Use wallpaper colours", "Match the system palette instead of the brand colours (Android 12+)", s.dynamicColor) { on ->
                        update { it.copy(dynamicColor = on) }
                    }
                }
            }
            UpdatesSection(s)
            Header("About")
            Row2("Version", BuildConfig.VERSION_NAME)
        }
    }

    if (lockDialog) {
        AlertDialog(
            onDismissRequest = { lockDialog = false },
            title = { Text("Lock after") },
            text = {
                Column {
                    listOf(1, 2, 5, 15).forEach { m ->
                        Row(
                            Modifier.fillMaxWidth().clickable {
                                update { it.copy(lockTimeoutMin = m) }
                                lockDialog = false
                            },
                            verticalAlignment = Alignment.CenterVertically,
                        ) {
                            RadioButton(selected = settings?.lockTimeoutMin == m, onClick = null)
                            Text("$m min", Modifier.padding(start = 12.dp), color = MaterialTheme.colorScheme.onSurface)
                        }
                    }
                }
            },
            confirmButton = { TextButton(onClick = { lockDialog = false }) { Text("Close") } },
        )
    }
    confirmRevoke?.let { dev ->
        AlertDialog(
            onDismissRequest = { confirmRevoke = null },
            title = { Text(if (dev.current) "Sign out this phone?" else "Revoke ${dev.name}?") },
            text = { Text("That device's tokens stop working immediately and it must sign in again.") },
            confirmButton = {
                TextButton(onClick = {
                    confirmRevoke = null
                    if (dev.current) container.logout() else vm.revoke(dev.id)
                }) { Text(if (dev.current) "Sign out" else "Revoke") }
            },
            dismissButton = { TextButton(onClick = { confirmRevoke = null }) { Text("Cancel") } },
        )
    }
    if (confirmLogout) {
        AlertDialog(
            onDismissRequest = { confirmLogout = false },
            title = { Text("Sign out?") },
            text = { Text("This phone is revoked on the server and the saved session is deleted.") },
            confirmButton = {
                TextButton(onClick = {
                    confirmLogout = false
                    container.logout()
                }) { Text("Sign out") }
            },
            dismissButton = { TextButton(onClick = { confirmLogout = false }) { Text("Cancel") } },
        )
    }
}

/** Update check: toggle, "Check for updates", last result and the update card. */
@Composable
private fun UpdatesSection(s: AppSettings?) {
    val container = LocalContainer.current
    val updates = container.updates
    val state by updates.state.collectAsStateWithLifecycle()
    val scope = rememberCoroutineScope()
    Header("Updates")
    Row2("Installed version", "${BuildConfig.VERSION_NAME} (build ${BuildConfig.VERSION_CODE})")
    if (!state.enabled) {
        Text(
            "Update checks are switched off in debug builds. This is the separate .debug app: install the signed release build to get updates.",
            style = MaterialTheme.typography.bodySmall,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
            modifier = Modifier.padding(horizontal = 16.dp, vertical = 4.dp),
        )
        return
    }
    if (s != null) {
        SwitchRow("Check for updates automatically", "When the app starts, at most every 6 hours", s.updateCheckEnabled) { on ->
            scope.launch { container.settings.update { it.copy(updateCheckEnabled = on) } }
        }
        Row2("Last check", if (s.updateLastCheckAt > 0) formatCheckTime(s.updateLastCheckAt) else "Never")
        if (s.updateLastResult.isNotBlank()) {
            Text(
                s.updateLastResult,
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                modifier = Modifier.padding(horizontal = 16.dp, vertical = 2.dp),
            )
        }
    }
    if (state.available == null) {
        state.error?.let {
            Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodyMedium, modifier = Modifier.padding(horizontal = 16.dp, vertical = 4.dp))
        }
    }
    Row(Modifier.padding(horizontal = 16.dp, vertical = 8.dp), verticalAlignment = Alignment.CenterVertically) {
        Button(onClick = updates::checkNow, enabled = !state.busy) { Text("Check for updates") }
        if (state.checking) LoadingSpinner(Modifier.padding(start = 12.dp))
    }
    if (state.available != null) UpdateCard(state, updates, Modifier.padding(horizontal = 16.dp, vertical = 4.dp))
}

private fun formatCheckTime(epochMillis: Long): String =
    android.text.format.DateUtils.getRelativeTimeSpanString(
        epochMillis,
        System.currentTimeMillis(),
        android.text.format.DateUtils.MINUTE_IN_MILLIS,
    ).toString()

private fun Modifier.height2(): Modifier = this.fillMaxWidth().padding(vertical = 8.dp)

@OptIn(ExperimentalLayoutApi::class)
@Composable
private fun DeviceRow(d: DeviceSession, revoking: Boolean, onRevoke: () -> Unit) {
    Row(Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 8.dp), verticalAlignment = Alignment.CenterVertically) {
        Column(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(2.dp)) {
            Text(
                d.name.ifBlank { d.model.ifBlank { "Unknown device" } },
                style = MaterialTheme.typography.titleSmall,
                color = MaterialTheme.colorScheme.onSurface,
                maxLines = 2,
                overflow = TextOverflow.Ellipsis,
            )
            if (d.current || d.revokedAt != null) {
                FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                    if (d.current) StatusChip("This phone", Tone.Good)
                    if (d.revokedAt != null) StatusChip("Revoked", Tone.Bad)
                }
            }
            Text(
                "Last seen ${formatAge(d.lastSeenAt)}${if (d.lastIp.isNotBlank()) " from ${d.lastIp}" else ""}",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            Text(
                "First signed in ${formatDateTime(d.createdAt)}${if (d.appVersion.isNotBlank()) ", app ${d.appVersion}" else ""}",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
        // The current phone is signed out with the Account "Sign out" button.
        if (d.revokedAt == null && !d.current) {
            if (revoking) LoadingSpinner(Modifier.padding(8.dp)) else TextButton(onClick = onRevoke) { Text("Revoke", maxLines = 1, softWrap = false) }
        }
    }
}

@Composable
private fun Header(text: String) {
    Text(
        text,
        style = MaterialTheme.typography.titleSmall,
        color = MaterialTheme.colorScheme.primary,
        modifier = Modifier.padding(start = 16.dp, end = 16.dp, top = 20.dp, bottom = 4.dp),
    )
}

@Composable
private fun Row2(label: String, value: String) {
    Row(Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 6.dp), horizontalArrangement = Arrangement.SpaceBetween) {
        Text(label, color = MaterialTheme.colorScheme.onSurfaceVariant, style = MaterialTheme.typography.bodyMedium, modifier = Modifier.weight(0.4f, fill = false))
        Text(value, color = MaterialTheme.colorScheme.onSurface, style = MaterialTheme.typography.bodyMedium, textAlign = androidx.compose.ui.text.style.TextAlign.End, modifier = Modifier.weight(0.6f).padding(start = 16.dp))
    }
}

@Composable
private fun SwitchRow(title: String, subtitle: String, checked: Boolean, onChange: (Boolean) -> Unit) {
    Row(
        Modifier.fillMaxWidth().clickable { onChange(!checked) }.padding(horizontal = 16.dp, vertical = 10.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Column(Modifier.weight(1f)) {
            Text(title, style = MaterialTheme.typography.bodyLarge, color = MaterialTheme.colorScheme.onSurface)
            Text(subtitle, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
        Switch(checked = checked, onCheckedChange = onChange)
    }
}

@Composable
private fun ActionRow(title: String, subtitle: String, onClick: () -> Unit) {
    Column(Modifier.fillMaxWidth().clickable(onClick = onClick).padding(horizontal = 16.dp, vertical = 10.dp)) {
        Text(title, style = MaterialTheme.typography.bodyLarge, color = MaterialTheme.colorScheme.onSurface)
        Text(subtitle, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
    }
}
