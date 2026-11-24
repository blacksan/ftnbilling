package ke.co.ftw.ftnadmin.ui.update

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.SystemUpdate
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.Icon
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalUriHandler
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.LifecycleResumeEffect
import ke.co.ftw.ftnadmin.data.update.UpdateManager
import ke.co.ftw.ftnadmin.data.update.UpdateState
import ke.co.ftw.ftnadmin.data.update.cleanNotes
import java.util.Locale

/**
 * "Update available" card for Home and Settings: notes, an Update button, download progress, the
 * install-permission explanation and errors with a retry. Draws nothing when there is no update.
 */
@Composable
fun UpdateCard(state: UpdateState, manager: UpdateManager, modifier: Modifier = Modifier) {
    val update = state.available ?: return
    // Back from the "install unknown apps" screen: continue by itself if the user allowed it.
    LifecycleResumeEffect(state.permissionNeeded) {
        if (state.permissionNeeded) manager.onResumed()
        onPauseOrDispose {}
    }
    val scheme = MaterialTheme.colorScheme
    val uriHandler = LocalUriHandler.current
    Card(
        modifier = modifier.fillMaxWidth(),
        shape = MaterialTheme.shapes.medium,
        colors = CardDefaults.cardColors(containerColor = scheme.primaryContainer, contentColor = scheme.onPrimaryContainer),
    ) {
        Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(10.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                Surface(shape = CircleShape, color = scheme.primary, contentColor = scheme.onPrimary, modifier = Modifier.size(40.dp)) {
                    Icon(Icons.Default.SystemUpdate, contentDescription = null, modifier = Modifier.padding(9.dp))
                }
                Text(
                    "Update available: v${update.version}",
                    style = MaterialTheme.typography.titleMedium,
                    modifier = Modifier.weight(1f),
                )
            }
            val notes = cleanNotes(update.notes)
            if (notes.isNotBlank()) Text(notes, style = MaterialTheme.typography.bodySmall)

            val download = state.download
            when {
                download != null -> {
                    val fraction = if (download.total > 0) (download.done.toFloat() / download.total).coerceIn(0f, 1f) else 0f
                    LinearProgressIndicator(progress = { fraction }, modifier = Modifier.fillMaxWidth())
                    Text(
                        if (download.done >= download.total) {
                            "Checking the download..."
                        } else {
                            "Downloading ${(fraction * 100).toInt()}% (${megabytes(download.done)} of ${megabytes(download.total)})"
                        },
                        style = MaterialTheme.typography.bodySmall,
                    )
                }
                state.permissionNeeded -> {
                    Text(
                        "To install updates Android needs your permission for this app to install apps. " +
                            "Tap Open settings, turn on \"Allow from this source\", then come back: the update continues by itself.",
                        style = MaterialTheme.typography.bodyMedium,
                    )
                    Button(onClick = manager::openInstallPermissionSettings) { Text("Open settings") }
                }
                else -> {
                    state.error?.let {
                        Surface(shape = MaterialTheme.shapes.small, color = scheme.errorContainer, contentColor = scheme.onErrorContainer) {
                            Text(it, style = MaterialTheme.typography.bodyMedium, modifier = Modifier.padding(10.dp))
                        }
                    }
                    state.manualUrl?.let { link ->
                        OutlinedButton(onClick = { uriHandler.openUri(link) }) { Text("Open release page") }
                    }
                    Button(onClick = manager::startUpdate) { Text(if (state.error != null) "Try again" else "Update") }
                }
            }
        }
    }
}

private fun megabytes(bytes: Long): String = String.format(Locale.US, "%.1f MB", bytes / 1_048_576.0)
