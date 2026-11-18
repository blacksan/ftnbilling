package ke.co.ftw.ftnadmin.ui.common

import android.content.ClipboardManager
import android.content.Context
import android.net.Uri
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.material3.Button
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.unit.dp
import com.google.zxing.client.android.Intents
import com.journeyapps.barcodescanner.ScanContract
import com.journeyapps.barcodescanner.ScanOptions
import ke.co.ftw.ftnadmin.tunnel.TunnelSummary
import ke.co.ftw.ftnadmin.ui.onboarding.SecureCaptureActivity
import java.io.ByteArrayOutputStream
import java.io.IOException

private const val MAX_CONFIG_BYTES = 16 * 1024

/**
 * Three ways to bring a tunnel config in: scan the QR printed by `ftnbilling-admin-peer`, pick the
 * `.conf` file, or paste from the clipboard. The text goes to [onConfigText]; validation lives there.
 */
@Composable
fun TunnelImportPanel(
    busy: Boolean,
    error: String?,
    onConfigText: (String) -> Unit,
    modifier: Modifier = Modifier,
) {
    SecureScreen()
    val context = LocalContext.current
    var localError by remember { mutableStateOf<String?>(null) }

    val scanner = rememberLauncherForActivityResult(ScanContract()) { result ->
        result.contents?.let { localError = null; onConfigText(it) }
    }
    val picker = rememberLauncherForActivityResult(ActivityResultContracts.OpenDocument()) { uri ->
        if (uri != null) {
            try {
                localError = null
                onConfigText(readSmallText(context, uri))
            } catch (e: IOException) {
                localError = e.message
            }
        }
    }

    Column(modifier.fillMaxWidth(), verticalArrangement = Arrangement.spacedBy(8.dp)) {
        Button(
            onClick = {
                val options = ScanOptions()
                    .setDesiredBarcodeFormats("QR_CODE")
                    .setCaptureActivity(SecureCaptureActivity::class.java)
                    .setOrientationLocked(true)
                    .setBeepEnabled(false)
                    .setPrompt("Scan the QR code from ftnbilling-admin-peer")
                    // The terminal prints the QR inverted (light on dark); accept both.
                    .addExtra(Intents.Scan.SCAN_TYPE, Intents.Scan.MIXED_SCAN)
                scanner.launch(options)
            },
            enabled = !busy,
            modifier = Modifier.fillMaxWidth(),
        ) { Text("Scan QR code") }
        OutlinedButton(onClick = { picker.launch(arrayOf("*/*")) }, enabled = !busy, modifier = Modifier.fillMaxWidth()) {
            Text("Choose .conf file")
        }
        OutlinedButton(
            onClick = {
                val text = readClipboard(context)
                if (text == null) {
                    localError = "The clipboard is empty."
                } else {
                    localError = null
                    onConfigText(text)
                }
            },
            enabled = !busy,
            modifier = Modifier.fillMaxWidth(),
        ) { Text("Paste from clipboard") }
        (error ?: localError)?.let {
            Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodyMedium)
        }
    }
}

/** What the owner may see after an import: never the key. */
@Composable
fun TunnelSummaryBlock(summary: TunnelSummary) {
    Column(verticalArrangement = Arrangement.spacedBy(2.dp)) {
        Text("This device: ${summary.address}", style = MaterialTheme.typography.bodyMedium)
        Text("Server: ${summary.endpoint}", style = MaterialTheme.typography.bodyMedium)
        Text("Routes only: ${summary.allowedIps.joinToString(", ")}", style = MaterialTheme.typography.bodyMedium)
    }
}

private fun readSmallText(context: Context, uri: Uri): String {
    val stream = context.contentResolver.openInputStream(uri) ?: throw IOException("Cannot open that file.")
    stream.use { input ->
        val out = ByteArrayOutputStream()
        val chunk = ByteArray(4096)
        while (true) {
            val n = input.read(chunk)
            if (n < 0) break
            if (out.size() + n > MAX_CONFIG_BYTES) throw IOException("That file is too large to be a tunnel config.")
            out.write(chunk, 0, n)
        }
        return out.toString(Charsets.UTF_8.name())
    }
}

private fun readClipboard(context: Context): String? {
    val clip = context.getSystemService(ClipboardManager::class.java)?.primaryClip ?: return null
    if (clip.itemCount == 0) return null
    return clip.getItemAt(0).coerceToText(context)?.toString()?.takeIf { it.isNotBlank() }
}
