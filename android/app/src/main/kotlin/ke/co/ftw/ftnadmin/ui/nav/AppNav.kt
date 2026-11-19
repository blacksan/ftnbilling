package ke.co.ftw.ftnadmin.ui.nav

import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import ke.co.ftw.ftnadmin.auth.LockState
import ke.co.ftw.ftnadmin.core.network.AuthState
import ke.co.ftw.ftnadmin.ui.common.LoadingSpinner
import ke.co.ftw.ftnadmin.ui.common.LocalContainer
import ke.co.ftw.ftnadmin.ui.lock.LockScreen
import ke.co.ftw.ftnadmin.ui.login.LoginScreen

/**
 * Root gate: loading, login, lock, then the main shell. Everything sits in a Surface so
 * LocalContentColor is onBackground and no text can fall back to black on the dark theme.
 */
@Composable
fun AppNav() {
    val container = LocalContainer.current
    val settings by container.settings.state.collectAsStateWithLifecycle()
    val auth by container.sessions.state.collectAsStateWithLifecycle()
    val lock by container.appLock.state.collectAsStateWithLifecycle()

    Surface(Modifier.fillMaxSize(), color = MaterialTheme.colorScheme.background) {
        val a = auth
        when {
            settings == null || a is AuthState.Loading || lock == LockState.Unknown ->
                Box(Modifier.fillMaxSize()) { LoadingSpinner(Modifier.align(Alignment.Center)) }
            a is AuthState.SignedOut -> LoginScreen()
            lock == LockState.Locked -> LockScreen()
            a is AuthState.SignedIn -> MainShell(a.user)
        }
    }
}
