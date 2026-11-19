package ke.co.ftw.ftnadmin

import android.os.Bundle
import android.view.WindowManager
import android.graphics.Color
import androidx.activity.SystemBarStyle
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.runtime.DisposableEffect
import androidx.appcompat.app.AppCompatActivity
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.core.splashscreen.SplashScreen.Companion.installSplashScreen
import androidx.lifecycle.lifecycleScope
import ke.co.ftw.ftnadmin.core.prefs.ThemeMode
import ke.co.ftw.ftnadmin.ui.common.LocalContainer
import ke.co.ftw.ftnadmin.ui.nav.AppNav
import ke.co.ftw.ftnadmin.ui.theme.AppTheme
import ke.co.ftw.ftnadmin.ui.theme.supportsDynamicTheming
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.launch

/** Single activity. AppCompat is only here because BiometricPrompt needs a FragmentActivity. */
class MainActivity : AppCompatActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        installSplashScreen()
        enableEdgeToEdge()
        super.onCreate(savedInstanceState)
        val container = (application as FtnApp).container

        // Secure by default until settings are known, so nothing leaks into the recents screenshot.
        window.setFlags(WindowManager.LayoutParams.FLAG_SECURE, WindowManager.LayoutParams.FLAG_SECURE)
        lifecycleScope.launch {
            combine(container.settings.state, container.windowSecurity.forced) { s, forced ->
                (s?.flagSecure ?: true) || forced > 0
            }.distinctUntilChanged().collect { secure ->
                if (secure) {
                    window.addFlags(WindowManager.LayoutParams.FLAG_SECURE)
                } else {
                    window.clearFlags(WindowManager.LayoutParams.FLAG_SECURE)
                }
            }
        }

        setContent {
            CompositionLocalProvider(LocalContainer provides container) {
                val settings by container.settings.state.collectAsState()
                // The chosen mode wins over the phone's setting, and the system bars follow it at runtime.
                val dark = (settings?.themeMode ?: ThemeMode.System).isDark(isSystemInDarkTheme())
                DisposableEffect(dark) {
                    enableEdgeToEdge(
                        statusBarStyle = SystemBarStyle.auto(Color.TRANSPARENT, Color.TRANSPARENT) { dark },
                        navigationBarStyle = SystemBarStyle.auto(LIGHT_SCRIM, DARK_SCRIM) { dark },
                    )
                    onDispose {}
                }
                AppTheme(darkTheme = dark, dynamicColor = (settings?.dynamicColor ?: false) && supportsDynamicTheming()) { AppNav() }
            }
        }
    }

    private companion object {
        val LIGHT_SCRIM = Color.argb(0xe6, 0xFF, 0xFF, 0xFF)
        val DARK_SCRIM = Color.argb(0x80, 0x1b, 0x1b, 0x1b)
    }
}
