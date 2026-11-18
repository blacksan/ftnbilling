package ke.co.ftw.ftnadmin.ui.common

import android.app.Activity
import android.app.KeyguardManager
import android.content.Context
import android.content.Intent
import android.net.Uri
import android.widget.Toast
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.Card
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.compositionLocalOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import ke.co.ftw.ftnadmin.AppContainer
import kotlinx.coroutines.launch

val LocalContainer = compositionLocalOf<AppContainer> { error("AppContainer not provided") }

/** Keeps FLAG_SECURE on while this screen is shown, whatever the user setting says. */
@Composable
fun SecureScreen() {
    val security = LocalContainer.current.windowSecurity
    DisposableEffect(security) {
        security.acquire()
        onDispose { security.release() }
    }
}

/**
 * Returns an action that connects the tunnel, showing the system VPN consent dialog first when needed.
 * Call it only while the activity is resumed.
 */
@Composable
fun rememberConnectAction(): () -> Unit {
    val container = LocalContainer.current
    val scope = rememberCoroutineScope()
    val launcher = rememberLauncherForActivityResult(ActivityResultContracts.StartActivityForResult()) { result ->
        if (result.resultCode == Activity.RESULT_OK) {
            scope.launch { container.tunnel.connect() }
        } else {
            container.tunnel.markNeedsPermission()
        }
    }
    return remember(container, scope, launcher) {
        {
            val intent = container.tunnel.prepareIntent()
            if (intent == null) scope.launch { container.tunnel.connect() } else launcher.launch(intent)
        }
    }
}

@Composable
fun SectionCard(title: String, modifier: Modifier = Modifier, content: @Composable () -> Unit) {
    Card(modifier = modifier.fillMaxWidth()) {
        Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
            Text(title, style = MaterialTheme.typography.titleMedium)
            content()
        }
    }
}

fun Context.isDeviceSecure(): Boolean = getSystemService(KeyguardManager::class.java)?.isDeviceSecure == true

fun Context.openInBrowser(uri: Uri) {
    try {
        startActivity(Intent(Intent.ACTION_VIEW, uri).addCategory(Intent.CATEGORY_BROWSABLE))
    } catch (_: android.content.ActivityNotFoundException) {
        Toast.makeText(this, "No app can open this link", Toast.LENGTH_SHORT).show()
    }
}
