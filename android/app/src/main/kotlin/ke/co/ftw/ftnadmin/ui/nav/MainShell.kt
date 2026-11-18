package ke.co.ftw.ftnadmin.ui.nav

import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.AccountBox
import androidx.compose.material.icons.filled.Home
import androidx.compose.material.icons.filled.Notifications
import androidx.compose.material.icons.filled.Person
import androidx.compose.material.icons.filled.Place
import androidx.compose.material3.Icon
import androidx.compose.material3.NavigationBar
import androidx.compose.material3.NavigationBarItem
import androidx.compose.material3.NavigationRail
import androidx.compose.material3.NavigationRailItem
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.platform.LocalConfiguration
import androidx.navigation.NavDestination.Companion.hierarchy
import androidx.navigation.NavGraph.Companion.findStartDestination
import androidx.navigation.NavHostController
import androidx.navigation.compose.NavHost
import androidx.navigation.compose.composable
import androidx.navigation.compose.currentBackStackEntryAsState
import androidx.navigation.compose.rememberNavController
import ke.co.ftw.ftnadmin.core.network.UserInfo
import ke.co.ftw.ftnadmin.ui.common.EmptyState
import ke.co.ftw.ftnadmin.ui.settings.SettingsScreen

object Routes {
    const val DASHBOARD = "dashboard"
    const val CUSTOMERS = "customers"
    const val PAYMENTS = "payments"
    const val BUILDINGS = "buildings"
    const val ALERTS = "alerts"
    const val SETTINGS = "settings"
}

private enum class Tab(val route: String, val label: String, val icon: ImageVector) {
    Dashboard(Routes.DASHBOARD, "Home", Icons.Default.Home),
    Customers(Routes.CUSTOMERS, "Customers", Icons.Default.Person),
    Payments(Routes.PAYMENTS, "Payments", Icons.Default.AccountBox),
    Buildings(Routes.BUILDINGS, "Buildings", Icons.Default.Place),
    Alerts(Routes.ALERTS, "Alerts", Icons.Default.Notifications),
}

/** Adaptive shell: bottom bar on phones, rail from 600 dp wide. */
@Composable
fun MainShell(user: UserInfo) {
    val nav = rememberNavController()
    val entry by nav.currentBackStackEntryAsState()
    val current = entry?.destination
    val showBars = Tab.entries.any { t -> current?.hierarchy?.any { it.route == t.route } == true }
    val wide = LocalConfiguration.current.screenWidthDp >= 600

    fun go(tab: Tab) = nav.navigate(tab.route) {
        popUpTo(nav.graph.findStartDestination().id) { saveState = true }
        launchSingleTop = true
        restoreState = true
    }

    Row(Modifier.fillMaxSize()) {
        if (wide && showBars) {
            NavigationRail {
                Tab.entries.forEach { t ->
                    NavigationRailItem(
                        selected = current?.hierarchy?.any { it.route == t.route } == true,
                        onClick = { go(t) },
                        icon = { Icon(t.icon, contentDescription = null) },
                        label = { Text(t.label) },
                    )
                }
            }
        }
        Column(Modifier.weight(1f)) {
            Box(Modifier.weight(1f)) { Destinations(nav, user) }
            if (!wide && showBars) {
                NavigationBar {
                    Tab.entries.forEach { t ->
                        NavigationBarItem(
                            selected = current?.hierarchy?.any { it.route == t.route } == true,
                            onClick = { go(t) },
                            icon = { Icon(t.icon, contentDescription = null) },
                            label = { Text(t.label) },
                        )
                    }
                }
            }
        }
    }
}

@Composable
private fun Destinations(nav: NavHostController, @Suppress("UNUSED_PARAMETER") user: UserInfo) {
    NavHost(nav, startDestination = Routes.DASHBOARD) {
        composable(Routes.DASHBOARD) { EmptyState("Dashboard") }
        composable(Routes.CUSTOMERS) { EmptyState("Customers") }
        composable(Routes.PAYMENTS) { EmptyState("Payments") }
        composable(Routes.BUILDINGS) { EmptyState("Buildings") }
        composable(Routes.ALERTS) { EmptyState("Alerts") }
        composable(Routes.SETTINGS) { SettingsScreen(onBack = { nav.popBackStack() }) }
    }
}
