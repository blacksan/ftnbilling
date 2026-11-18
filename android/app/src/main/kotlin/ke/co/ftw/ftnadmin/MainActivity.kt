package ke.co.ftw.ftnadmin

import android.os.Bundle
import android.view.WindowManager
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.appcompat.app.AppCompatActivity
import androidx.compose.runtime.CompositionLocalProvider
import androidx.lifecycle.lifecycleScope
import ke.co.ftw.ftnadmin.ui.common.LocalContainer
import ke.co.ftw.ftnadmin.ui.nav.AppNav
import ke.co.ftw.ftnadmin.ui.theme.FtnTheme
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.launch

/** Single activity. AppCompat is only here because BiometricPrompt needs a FragmentActivity. */
class MainActivity : AppCompatActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
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
                FtnTheme { AppNav() }
            }
        }
    }
}
