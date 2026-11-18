package ke.co.ftw.ftnadmin.ui.common

import android.app.KeyguardManager
import android.content.Context
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material.icons.filled.Settings
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.ExperimentalMaterial3ExpressiveApi
import androidx.compose.material3.LoadingIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.compositionLocalOf
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.dp
import ke.co.ftw.ftnadmin.AppContainer
import ke.co.ftw.ftnadmin.ui.theme.AppTheme

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

/** Expressive indeterminate indicator for loading states. */
@OptIn(ExperimentalMaterial3ExpressiveApi::class)
@Composable
fun LoadingSpinner(modifier: Modifier = Modifier) {
    LoadingIndicator(modifier)
}

@Composable
fun CenteredLoading(modifier: Modifier = Modifier) {
    Box(modifier.fillMaxSize(), contentAlignment = Alignment.Center) { LoadingSpinner() }
}

@Composable
fun ErrorState(message: String, onRetry: (() -> Unit)?, modifier: Modifier = Modifier) {
    Column(
        modifier.fillMaxSize().padding(24.dp),
        verticalArrangement = Arrangement.Center,
        horizontalAlignment = Alignment.CenterHorizontally,
    ) {
        Text(
            message,
            style = MaterialTheme.typography.bodyLarge,
            color = MaterialTheme.colorScheme.onSurface,
            textAlign = TextAlign.Center,
        )
        if (onRetry != null) {
            Button(onClick = onRetry, modifier = Modifier.padding(top = 16.dp)) { Text("Try again") }
        }
    }
}

@Composable
fun EmptyState(message: String, modifier: Modifier = Modifier) {
    Box(modifier.fillMaxSize().padding(24.dp), contentAlignment = Alignment.Center) {
        Text(
            message,
            style = MaterialTheme.typography.bodyLarge,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
            textAlign = TextAlign.Center,
        )
    }
}

@Composable
fun SectionCard(title: String, modifier: Modifier = Modifier, content: @Composable () -> Unit) {
    Card(
        modifier = modifier.fillMaxWidth(),
        colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceContainer),
    ) {
        Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
            Text(title, style = MaterialTheme.typography.titleMedium, color = MaterialTheme.colorScheme.onSurface)
            content()
        }
    }
}

@Composable
fun LabeledValue(label: String, value: String, modifier: Modifier = Modifier) {
    Row(modifier.fillMaxWidth(), horizontalArrangement = Arrangement.SpaceBetween) {
        Text(label, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
        Text(
            value.ifBlank { "-" },
            style = MaterialTheme.typography.bodyMedium,
            color = MaterialTheme.colorScheme.onSurface,
            textAlign = TextAlign.End,
            modifier = Modifier.padding(start = 16.dp),
        )
    }
}

enum class Tone { Neutral, Good, Warn, Bad }

/** Small coloured status pill using only theme colours. */
@Composable
fun StatusChip(text: String, tone: Tone, modifier: Modifier = Modifier) {
    val ext = AppTheme.extended
    val scheme = MaterialTheme.colorScheme
    val (bg: Color, fg: Color) = when (tone) {
        Tone.Good -> ext.successContainer to ext.onSuccessContainer
        Tone.Warn -> ext.warningContainer to ext.onWarningContainer
        Tone.Bad -> scheme.errorContainer to scheme.onErrorContainer
        Tone.Neutral -> scheme.secondaryContainer to scheme.onSecondaryContainer
    }
    Surface(modifier, shape = MaterialTheme.shapes.small, color = bg, contentColor = fg) {
        Text(
            text,
            style = MaterialTheme.typography.labelMedium,
            modifier = Modifier.padding(horizontal = 8.dp, vertical = 2.dp),
        )
    }
}

fun Context.isDeviceSecure(): Boolean = getSystemService(KeyguardManager::class.java)?.isDeviceSecure == true

@Composable
fun FullWidth(content: @Composable () -> Unit) {
    Box(Modifier.fillMaxWidth()) { content() }
}

/** Top bar shared by the top-level screens: title plus a settings shortcut. */
@OptIn(androidx.compose.material3.ExperimentalMaterial3Api::class)
@Composable
fun AppTopBar(
    title: String,
    onSettings: (() -> Unit)? = null,
    onBack: (() -> Unit)? = null,
    actions: @Composable androidx.compose.foundation.layout.RowScope.() -> Unit = {},
) {
    androidx.compose.material3.TopAppBar(
        title = { Text(title, maxLines = 1, overflow = androidx.compose.ui.text.style.TextOverflow.Ellipsis) },
        navigationIcon = {
            if (onBack != null) {
                androidx.compose.material3.IconButton(onClick = onBack) {
                    androidx.compose.material3.Icon(Icons.AutoMirrored.Filled.ArrowBack, contentDescription = "Back")
                }
            }
        },
        actions = {
            actions()
            if (onSettings != null) {
                androidx.compose.material3.IconButton(onClick = onSettings) {
                    androidx.compose.material3.Icon(Icons.Default.Settings, contentDescription = "Settings")
                }
            }
        },
    )
}
