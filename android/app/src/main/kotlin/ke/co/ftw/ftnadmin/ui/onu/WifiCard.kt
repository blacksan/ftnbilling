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
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.text.input.VisualTransformation
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import ke.co.ftw.ftnadmin.data.onu.Onu
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
                    OnuBody(onu, o.value.task?.let { TaskLine(it.statusText, it.isQueued, it.isFailed) }, ui.error, canWrite, ui.busy, vm)
                }
            }
        }
    }
    if (ui.sheet) ChangeWifiSheet(ui.form, ui.busy, ui.error, vm)
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

private data class TaskLine(val text: String, val queued: Boolean, val failed: Boolean)

@OptIn(ExperimentalLayoutApi::class)
@Composable
private fun OnuBody(onu: Onu, task: TaskLine?, error: String?, canWrite: Boolean, busy: Boolean, vm: WifiViewModel) {
    FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
        StatusChip(if (onu.online) "ONU online" else "ONU silent", if (onu.online) Tone.Good else Tone.Warn)
        if (onu.desired.changedOnDevice) StatusChip("Changed on the device", Tone.Warn)
    }
    LabeledValue("ONU", onu.title)
    LabeledValue("Serial", onu.serial)
    onu.warnings.forEach { Text(it, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.error) }
    if (onu.wifi.isEmpty()) {
        Text("The ONU has not reported its Wi-Fi yet.", style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
    }
    onu.wifi.forEach { w -> LabeledValue("${w.band} GHz", if (w.enabled) w.ssid else "${w.ssid} (off)") }
    if (onu.desired.changedOnDevice) {
        Text(
            "The Wi-Fi name on the box is not the one saved here. The tenant may have changed it. Saving again overwrites it.",
            style = MaterialTheme.typography.bodySmall,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
    }
    task?.let {
        Row(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalAlignment = Alignment.CenterVertically) {
            Text("Last request", style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
            StatusChip(it.text, if (it.failed) Tone.Bad else if (it.queued) Tone.Neutral else Tone.Good)
        }
    }
    error?.let { Text(it, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.error) }
    FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
        OutlinedButton(onClick = vm::refresh, enabled = !busy) { Text("Refresh") }
        if (canWrite) {
            Button(onClick = vm::openSheet, enabled = !busy) { Text("Change Wi-Fi") }
            FilledTonalButton(onClick = vm::askReboot, enabled = !busy) { Text("Reboot ONU") }
        }
    }
}

/** The Change Wi-Fi sheet: name and password, the same-for-both switch, and a show/hide control on the password boxes. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun ChangeWifiSheet(form: WifiForm, busy: Boolean, error: String?, vm: WifiViewModel) {
    val hide = if (form.showPassword) VisualTransformation.None else PasswordVisualTransformation()
    ModalBottomSheet(onDismissRequest = vm::closeSheet, sheetState = rememberModalBottomSheetState(skipPartiallyExpanded = true)) {
        Column(
            Modifier.fillMaxWidth().verticalScroll(rememberScrollState()).padding(horizontal = 16.dp, vertical = 8.dp),
            verticalArrangement = Arrangement.spacedBy(10.dp),
        ) {
            Text("Change Wi-Fi", style = MaterialTheme.typography.titleLarge, color = MaterialTheme.colorScheme.onSurface)
            Text(
                "The current password is never shown. The new one is stored encrypted so it can be put back after a factory reset.",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            OutlinedTextField(
                form.ssid, vm::onSsid, label = { Text(if (form.same) "Wi-Fi name" else "2.4 GHz name") }, singleLine = true,
                isError = form.errors["ssid"] != null, supportingText = { form.errors["ssid"]?.let { Text(it) } }, modifier = Modifier.fillMaxWidth(),
            )
            OutlinedTextField(
                form.password, vm::onPassword, label = { Text(if (form.same) "Password" else "2.4 GHz password") }, singleLine = true,
                visualTransformation = hide, keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Password),
                isError = form.errors["password"] != null, supportingText = { form.errors["password"]?.let { Text(it) } ?: Text("8 to 63 characters") },
                modifier = Modifier.fillMaxWidth(),
            )
            Row(verticalAlignment = Alignment.CenterVertically) {
                Checkbox(form.same, vm::onSame)
                Text("Same name and password on 2.4 and 5 GHz", style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurface)
            }
            if (!form.same) {
                OutlinedTextField(
                    form.ssid5, vm::onSsid5, label = { Text("5 GHz name") }, singleLine = true,
                    isError = form.errors["ssid5"] != null, supportingText = { form.errors["ssid5"]?.let { Text(it) } }, modifier = Modifier.fillMaxWidth(),
                )
                OutlinedTextField(
                    form.password5, vm::onPassword5, label = { Text("5 GHz password") }, singleLine = true,
                    visualTransformation = hide, keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Password),
                    isError = form.errors["password5"] != null, supportingText = { form.errors["password5"]?.let { Text(it) } },
                    modifier = Modifier.fillMaxWidth(),
                )
            }
            Row(verticalAlignment = Alignment.CenterVertically) {
                Checkbox(form.showPassword, { vm.toggleShowPassword() })
                Text("Show the password while typing", style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurface)
            }
            error?.let { Text(it, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.error) }
            Button(onClick = vm::submit, enabled = !busy, modifier = Modifier.fillMaxWidth()) { Text(if (busy) "Sending..." else "Save and apply") }
            TextButton(onClick = vm::closeSheet, modifier = Modifier.fillMaxWidth()) { Text("Cancel") }
        }
    }
}
