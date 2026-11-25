package ke.co.ftw.ftnadmin.ui.onu

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.Checkbox
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FilledTonalButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.ModalBottomSheet
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.rememberModalBottomSheetState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.ui.res.stringResource
import androidx.compose.foundation.layout.imePadding
import ke.co.ftw.ftnadmin.R
import ke.co.ftw.ftnadmin.ui.common.CheckRow
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import ke.co.ftw.ftnadmin.data.onu.Onu
import ke.co.ftw.ftnadmin.data.onu.OnuTask
import ke.co.ftw.ftnadmin.data.onu.WifiReveal
import ke.co.ftw.ftnadmin.ui.common.LabeledValue
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.common.SectionCard
import ke.co.ftw.ftnadmin.ui.common.StatusChip
import ke.co.ftw.ftnadmin.ui.common.Tone

/** The "Wi-Fi" card of a house: the names the ONU reports, the state of the last request, Change Wi-Fi and Reboot ONU. */
@OptIn(ExperimentalLayoutApi::class)
@Composable
fun WifiCard(vm: WifiViewModel, canWrite: Boolean, modifier: Modifier = Modifier) {
    val ui by vm.state.collectAsStateWithLifecycle()
    SectionCard("Wi-Fi and ONU", modifier) {
        when (val o = ui.onu) {
            Loadable.Loading -> Text("Loading...", style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
            is Loadable.Failed -> {
                Text(o.message, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.error)
                TextButton(onClick = vm::load) { Text("Try again") }
            }
            is Loadable.Ready -> {
                val onu = o.value.onu
                if (onu == null) {
                    Text(
                        "No ONU is linked to this house yet. It links by itself when its serial is in the register for this house, or open the router's ONUs to link one.",
                        style = MaterialTheme.typography.bodyMedium,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                } else {
                    OnuBody(onu, o.value.task ?: onu.lastTask, ui.error, canWrite, ui.busy, vm)
                }
            }
        }
    }
    if (ui.sheet) ChangeWifiSheet(ui.form, (ui.onu as? Loadable.Ready)?.value?.onu?.has5ghz ?: true, ui.busy, ui.error, vm)
    ui.reveal?.let { RevealDialog(it, vm::closeReveal) }
    if (ui.confirmReboot) {
        AlertDialog(
            onDismissRequest = vm::cancelReboot,
            title = { Text("Reboot the ONU?") },
            text = { Text("The house loses internet for a minute or two.") },
            confirmButton = { TextButton(onClick = vm::reboot, enabled = !ui.busy) { Text("Reboot") } },
            dismissButton = { TextButton(onClick = vm::cancelReboot) { Text("Cancel") } },
        )
    }
}

@OptIn(ExperimentalLayoutApi::class)
@Composable
private fun OnuBody(onu: Onu, task: OnuTask?, error: String?, canWrite: Boolean, busy: Boolean, vm: WifiViewModel) {
    FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
        StatusChip(if (onu.online) "ONU online" else "ONU silent", if (onu.online) Tone.Good else Tone.Warn)
        if (onu.desired.changedOnDevice) StatusChip("Changed on the device", Tone.Warn)
        if (onu.isQuarantined) StatusChip("Held back: unexpected place", Tone.Bad)
        if (onu.isWaitingForMac) StatusChip("Waiting for MAC", Tone.Bad)
        if (onu.sharedLoginOnly) StatusChip("Shared login only", Tone.Neutral)
        if (onu.returned) StatusChip("Returned: reset on next check-in", Tone.Neutral)
    }
    if (onu.isQuarantined) {
        Text(
            "This ONU checked in from an unexpected place and gets nothing until the owner approves or blocks it in the router's ONUs.",
            style = MaterialTheme.typography.bodySmall,
            color = MaterialTheme.colorScheme.error,
        )
    }
    onu.macWait?.let { w ->
        Text(
            "${w.text}. Nothing is sent to it yet. An owner can trust it in the router's ONUs.",
            style = MaterialTheme.typography.bodySmall,
            color = MaterialTheme.colorScheme.error,
        )
    }
    LabeledValue("ONU", onu.title)
    LabeledValue(stringResource(R.string.serial), onu.serial, mono = true)
    onu.warnings.forEach { Text(it, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.error) }
    if (onu.wifi.isEmpty()) {
        Text("The ONU has not reported its Wi-Fi yet.", style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
    }
    onu.wifi.forEach { w -> LabeledValue("${w.band} GHz", if (w.enabled) w.ssid else "${w.ssid} (off)") }
    if (!onu.has5ghz && onu.wifi.isNotEmpty()) {
        Text("One 2.4 GHz radio. One name and password.", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
    }
    if (onu.desired.changedOnDevice) {
        Text(
            "The Wi-Fi name on the box is not the one saved here. The tenant may have changed it. Saving again overwrites it.",
            style = MaterialTheme.typography.bodySmall,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
    }
    task?.let {
        Row(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalAlignment = Alignment.CenterVertically) {
            Text(stringResource(R.string.last_request), style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
            StatusChip(it.statusText, if (it.isFailed) Tone.Bad else if (it.isExpired) Tone.Warn else if (it.isQueued || it.isAccepted) Tone.Neutral else Tone.Good)
        }
        if (it.isAccepted) {
            Text(
                "Accepted only means the box said yes. Check the Wi-Fi on a phone.",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
    }
    error?.let { Text(it, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.error) }
    FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
        OutlinedButton(onClick = vm::refresh, enabled = !busy) { Text("Refresh") }
        if (canWrite && task != null && task.canConfirm) {
            Button(onClick = vm::confirm, enabled = !busy) { Text("It works") }
            OutlinedButton(onClick = vm::reject, enabled = !busy) { Text("It didn't work") }
        }
        if (canWrite && task != null && task.canCancel) {
            OutlinedButton(onClick = { vm.cancelTask(task.id) }, enabled = !busy) { Text("Cancel request") }
        }
        if (canWrite) {
            OutlinedButton(onClick = vm::showPassword, enabled = !busy) { Text("Show Wi-Fi password") }
            Button(onClick = vm::openSheet, enabled = !busy) { Text("Change Wi-Fi") }
            FilledTonalButton(onClick = vm::askReboot, enabled = !busy) { Text("Reboot ONU") }
        }
    }
}

/** The Change Wi-Fi sheet: name and password (shown as typed, to avoid typos), and the 5 GHz options only when the ONU has a 5 GHz radio. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun ChangeWifiSheet(form: WifiForm, has5ghz: Boolean, busy: Boolean, error: String?, vm: WifiViewModel) {
    val both = form.same || !has5ghz
    ModalBottomSheet(onDismissRequest = vm::closeSheet, sheetState = rememberModalBottomSheetState(skipPartiallyExpanded = true)) {
        Column(
            Modifier.fillMaxWidth().imePadding().verticalScroll(rememberScrollState()).padding(horizontal = 16.dp).padding(top = 8.dp, bottom = 16.dp),
            verticalArrangement = Arrangement.spacedBy(10.dp),
        ) {
            Text("Change Wi-Fi", style = MaterialTheme.typography.titleLarge, color = MaterialTheme.colorScheme.onSurface)
            Text(
                "Saved encrypted. It is put back if the ONU is reset.",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            OutlinedTextField(
                form.ssid, vm::onSsid, label = { Text(if (both) "Wi-Fi name" else "2.4 GHz name") }, singleLine = true,
                isError = form.errors["ssid"] != null, supportingText = { form.errors["ssid"]?.let { Text(it) } }, modifier = Modifier.fillMaxWidth(),
            )
            OutlinedTextField(
                form.password, vm::onPassword, label = { Text(if (both) "New password" else "2.4 GHz password") }, singleLine = true,
                keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Ascii),
                isError = form.errors["password"] != null, supportingText = { form.errors["password"]?.let { Text(it) } ?: Text("8 to 63 characters") },
                modifier = Modifier.fillMaxWidth(),
            )
            if (has5ghz) {
                CheckRow(form.same, vm::onSame, stringResource(R.string.same_wifi_both_bands))
                if (!form.same) {
                    OutlinedTextField(
                        form.ssid5, vm::onSsid5, label = { Text("5 GHz name") }, singleLine = true,
                        isError = form.errors["ssid5"] != null, supportingText = { form.errors["ssid5"]?.let { Text(it) } }, modifier = Modifier.fillMaxWidth(),
                    )
                    OutlinedTextField(
                        form.password5, vm::onPassword5, label = { Text("5 GHz password") }, singleLine = true,
                        keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Ascii),
                        isError = form.errors["password5"] != null, supportingText = { form.errors["password5"]?.let { Text(it) } },
                        modifier = Modifier.fillMaxWidth(),
                    )
                }
            }
            error?.let { Text(it, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.error) }
            Button(onClick = vm::submit, enabled = !busy, modifier = Modifier.fillMaxWidth()) { Text(if (busy) "Sending..." else "Save and apply") }
            TextButton(onClick = vm::closeSheet, modifier = Modifier.fillMaxWidth()) { Text("Cancel") }
        }
    }
}

/** The current Wi-Fi password of the house, in the clear: it is the customer's own Wi-Fi key. Closing forgets it. */
@Composable
private fun RevealDialog(reveal: WifiReveal, onClose: () -> Unit) {
    AlertDialog(
        onDismissRequest = onClose,
        title = { Text("Wi-Fi password") },
        text = {
            Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                reveal.networks.forEach { k ->
                    val band = when (k.band) {
                        "2.4" -> "2.4 GHz"
                        "5" -> "5 GHz"
                        else -> "Wi-Fi"
                    }
                    Text(if (k.ssid.isBlank()) band else "$band: ${k.ssid}", style = MaterialTheme.typography.labelLarge, color = MaterialTheme.colorScheme.onSurfaceVariant)
                    SelectionContainer {
                        Text(
                            k.password.ifBlank { "No password known" },
                            style = MaterialTheme.typography.titleMedium,
                            fontFamily = FontFamily.Monospace,
                            color = MaterialTheme.colorScheme.onSurface,
                        )
                    }
                    Text(
                        when (k.source) {
                            "onu" -> "Read from the ONU."
                            "saved" -> "Saved here."
                            else -> ""
                        },
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
                if (reveal.note.isNotBlank()) {
                    Text(reveal.note, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                }
            }
        },
        confirmButton = { TextButton(onClick = onClose) { Text("Close") } },
    )
}
