package ke.co.ftw.ftnadmin.ui.nav

import ke.co.ftw.ftnadmin.ui.common.LoadingSpinner
import android.net.Uri
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.material3.MaterialTheme
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.navigation.NavType
import androidx.navigation.compose.NavHost
import androidx.navigation.compose.composable
import androidx.navigation.compose.rememberNavController
import androidx.navigation.navArgument
import ke.co.ftw.ftnadmin.auth.LockState
import ke.co.ftw.ftnadmin.ui.admin.AdminScreen
import ke.co.ftw.ftnadmin.ui.common.LocalContainer
import ke.co.ftw.ftnadmin.ui.home.HomeScreen
import ke.co.ftw.ftnadmin.ui.lock.LockScreen
import ke.co.ftw.ftnadmin.ui.onboarding.OnboardingScreen
import ke.co.ftw.ftnadmin.ui.settings.SettingsScreen

object Routes {
    const val ONBOARDING = "onboarding"
    const val LOCK = "lock"
    const val HOME = "home"
    const val ADMIN = "admin"
    const val SETTINGS = "settings"
    const val ADMIN_PATTERN = "$ADMIN?path={path}"

    fun admin(path: String? = null) = if (path == null) ADMIN else "$ADMIN?path=${Uri.encode(path)}"
}

@Composable
fun AppNav() {
    val container = LocalContainer.current
    val settings by container.settings.state.collectAsStateWithLifecycle()
    val loaded by container.tunnelRepository.loaded.collectAsStateWithLifecycle()
    val summary by container.tunnelRepository.summary.collectAsStateWithLifecycle()
    val lock by container.appLock.state.collectAsStateWithLifecycle()

    val s = settings
    Box(Modifier.fillMaxSize().background(MaterialTheme.colorScheme.background)) {
        if (s == null || !loaded || lock == LockState.Unknown) {
            LoadingSpinner(Modifier.align(Alignment.Center))
        } else {
            val gate = when {
                !s.onboardingDone || summary == null -> Routes.ONBOARDING
                lock == LockState.Locked -> Routes.LOCK
                else -> Routes.HOME
            }
            NavContent(gate)
        }
    }
}

@Composable
private fun NavContent(gate: String) {
    val nav = rememberNavController()
    val start = remember { gate }

    LaunchedEffect(gate) {
        val current = nav.currentDestination?.route?.substringBefore('?') ?: return@LaunchedEffect
        if (current != gate) {
            nav.navigate(gate) {
                popUpTo(nav.graph.id) { inclusive = true }
                launchSingleTop = true
            }
        }
    }

    NavHost(nav, startDestination = start) {
        composable(Routes.ONBOARDING) { OnboardingScreen() }
        composable(Routes.LOCK) { LockScreen() }
        composable(Routes.HOME) {
            HomeScreen(
                onOpenAdmin = { path -> nav.navigate(Routes.admin(path)) },
                onOpenSettings = { nav.navigate(Routes.SETTINGS) },
            )
        }
        composable(
            Routes.ADMIN_PATTERN,
            arguments = listOf(
                navArgument("path") {
                    type = NavType.StringType
                    nullable = true
                    defaultValue = null
                },
            ),
        ) { entry ->
            AdminScreen(
                path = entry.arguments?.getString("path"),
                onHome = { if (!nav.popBackStack(Routes.HOME, inclusive = false)) nav.navigate(Routes.HOME) },
            )
        }
        composable(Routes.SETTINGS) { SettingsScreen(onBack = { nav.popBackStack() }) }
    }
}
