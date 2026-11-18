package ke.co.ftw.ftnadmin.ui.nav

import android.net.Uri
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
import androidx.navigation.NavType
import androidx.navigation.navArgument
import ke.co.ftw.ftnadmin.ui.alerts.AlertsScreen
import ke.co.ftw.ftnadmin.ui.buildings.BuildingDetailScreen
import ke.co.ftw.ftnadmin.ui.buildings.BuildingsScreen
import ke.co.ftw.ftnadmin.ui.customers.CustomerDetailScreen
import ke.co.ftw.ftnadmin.ui.customers.CustomersScreen
import ke.co.ftw.ftnadmin.ui.dashboard.DashboardScreen
import ke.co.ftw.ftnadmin.ui.newdevices.NewDevicesScreen
import ke.co.ftw.ftnadmin.ui.payments.PaymentFilter
import ke.co.ftw.ftnadmin.ui.payments.PaymentsScreen
import ke.co.ftw.ftnadmin.ui.settings.SettingsScreen

object Routes {
    const val DASHBOARD = "dashboard"
    const val CUSTOMERS = "customers"
    const val PAYMENTS = "payments"
    const val BUILDINGS = "buildings"
    const val ALERTS = "alerts"
    const val SETTINGS = "settings"
    const val PAYMENTS_UNMATCHED = "payments/unmatched"
    const val CUSTOMER_PATTERN = "customer/{id}"
    const val BUILDING_PATTERN = "building/{id}"
    const val NEW_DEVICES_PATTERN = "newdevices?site={site}"

    fun customer(id: String) = "customer/${Uri.encode(id)}"

    fun building(id: String) = "building/${Uri.encode(id)}"

    fun newDevices(siteId: String?) = if (siteId == null) "newdevices" else "newdevices?site=${Uri.encode(siteId)}"
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
private fun Destinations(nav: NavHostController, user: UserInfo) {
    val openSettings = { nav.navigate(Routes.SETTINGS) }
    val openAccount = { id: String -> nav.navigate(Routes.customer(id)) }
    NavHost(nav, startDestination = Routes.DASHBOARD) {
        composable(Routes.DASHBOARD) {
            DashboardScreen(
                onSettings = openSettings,
                onNewDevices = { nav.navigate(Routes.newDevices(null)) },
                onUnmatchedPayments = { nav.navigate(Routes.PAYMENTS_UNMATCHED) },
                onAlerts = { nav.navigate(Routes.ALERTS) { launchSingleTop = true } },
            )
        }
        composable(Routes.CUSTOMERS) { CustomersScreen(onSettings = openSettings, onOpen = openAccount) }
        composable(Routes.PAYMENTS) { PaymentsScreen(user, onSettings = openSettings) }
        composable(Routes.PAYMENTS_UNMATCHED) { PaymentsScreen(user, onSettings = openSettings, initialFilter = PaymentFilter.Unmatched) }
        composable(Routes.BUILDINGS) { BuildingsScreen(onSettings = openSettings, onOpen = { nav.navigate(Routes.building(it)) }) }
        composable(Routes.ALERTS) { AlertsScreen(user, onSettings = openSettings) }
        composable(Routes.SETTINGS) { SettingsScreen(onBack = { nav.popBackStack() }) }
        composable(
            Routes.CUSTOMER_PATTERN,
            arguments = listOf(navArgument("id") { type = NavType.StringType }),
        ) { e ->
            CustomerDetailScreen(
                id = e.arguments?.getString("id").orEmpty(),
                user = user,
                onBack = { nav.popBackStack() },
                onOpenOther = openAccount,
            )
        }
        composable(
            Routes.BUILDING_PATTERN,
            arguments = listOf(navArgument("id") { type = NavType.StringType }),
        ) { e ->
            BuildingDetailScreen(
                id = e.arguments?.getString("id").orEmpty(),
                onBack = { nav.popBackStack() },
                onNewDevices = { nav.navigate(Routes.newDevices(it)) },
                onOpenAccount = openAccount,
            )
        }
        composable(
            Routes.NEW_DEVICES_PATTERN,
            arguments = listOf(
                navArgument("site") {
                    type = NavType.StringType
                    nullable = true
                    defaultValue = null
                },
            ),
        ) { e ->
            NewDevicesScreen(
                siteId = e.arguments?.getString("site"),
                user = user,
                onBack = { nav.popBackStack() },
                onOpenAccount = openAccount,
            )
        }
    }
}
