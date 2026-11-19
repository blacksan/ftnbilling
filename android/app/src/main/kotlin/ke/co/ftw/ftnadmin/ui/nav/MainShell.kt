package ke.co.ftw.ftnadmin.ui.nav

import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.AccountBox
import androidx.compose.material.icons.filled.Home
import androidx.compose.material.icons.filled.Menu
import androidx.compose.material.icons.filled.Person
import androidx.compose.material.icons.filled.Share
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.material3.adaptive.navigationsuite.NavigationSuiteScaffold
import androidx.compose.material3.adaptive.navigationsuite.NavigationSuiteType
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.platform.LocalConfiguration
import androidx.compose.ui.text.font.FontWeight
import androidx.navigation.NavDestination.Companion.hasRoute
import androidx.navigation.NavDestination.Companion.hierarchy
import androidx.navigation.NavGraph.Companion.findStartDestination
import androidx.navigation.NavHostController
import androidx.navigation.compose.NavHost
import androidx.navigation.compose.composable
import androidx.navigation.compose.currentBackStackEntryAsState
import androidx.navigation.compose.rememberNavController
import androidx.navigation.toRoute
import ke.co.ftw.ftnadmin.core.network.UserInfo
import ke.co.ftw.ftnadmin.ui.alerts.AlertsScreen
import ke.co.ftw.ftnadmin.ui.apartments.ApartmentDetailScreen
import ke.co.ftw.ftnadmin.ui.apartments.ApartmentFormScreen
import ke.co.ftw.ftnadmin.ui.apartments.ApartmentsScreen
import ke.co.ftw.ftnadmin.ui.common.LocalContainer
import ke.co.ftw.ftnadmin.ui.customers.CustomerDetailScreen
import ke.co.ftw.ftnadmin.ui.customers.CustomersScreen
import ke.co.ftw.ftnadmin.ui.customers.add.AddCustomerScreen
import ke.co.ftw.ftnadmin.ui.home.HomeScreen
import ke.co.ftw.ftnadmin.ui.more.AddRouterScreen
import ke.co.ftw.ftnadmin.ui.more.EditRouterScreen
import ke.co.ftw.ftnadmin.ui.more.MoreScreen
import ke.co.ftw.ftnadmin.ui.network.NetworkScreen
import ke.co.ftw.ftnadmin.ui.newdevices.NewDevicesScreen
import ke.co.ftw.ftnadmin.ui.payments.PaymentFilter
import ke.co.ftw.ftnadmin.ui.payments.PaymentsScreen
import ke.co.ftw.ftnadmin.ui.routers.RouterDetailScreen
import ke.co.ftw.ftnadmin.ui.routers.RoutersScreen
import ke.co.ftw.ftnadmin.ui.routers.setup.SetupScreen
import ke.co.ftw.ftnadmin.ui.settings.SettingsScreen
import kotlin.reflect.KClass

private enum class Tab(val route: Any, val type: KClass<*>, val label: String, val icon: ImageVector) {
    Home(HomeRoute, HomeRoute::class, "Home", Icons.Default.Home),
    Network(NetworkRoute, NetworkRoute::class, "Network", Icons.Default.Share),
    Customers(CustomersRoute, CustomersRoute::class, "Customers", Icons.Default.Person),
    Payments(PaymentsRoute(), PaymentsRoute::class, "Payments", Icons.Default.AccountBox),
    More(MoreRoute, MoreRoute::class, "More", Icons.Default.Menu),
}

/** Adaptive shell: NavigationSuiteScaffold gives a bottom bar on phones and a rail from 600 dp wide. */
@Composable
fun MainShell(user: UserInfo) {
    val container = LocalContainer.current
    LaunchedEffect(Unit) { container.routerContext.refresh() }

    val nav = rememberNavController()
    val entry by nav.currentBackStackEntryAsState()
    val destination = entry?.destination
    fun isSelected(t: Tab) = destination?.hierarchy?.any { it.hasRoute(t.type) } == true
    val showBars = Tab.entries.any(::isSelected)
    val wide = LocalConfiguration.current.screenWidthDp >= 600
    val layout = when {
        !showBars -> NavigationSuiteType.None
        wide -> NavigationSuiteType.NavigationRail
        else -> NavigationSuiteType.NavigationBar
    }

    fun go(tab: Tab) = nav.navigate(tab.route) {
        popUpTo(nav.graph.findStartDestination().id) { saveState = true }
        launchSingleTop = true
        restoreState = true
    }

    NavigationSuiteScaffold(
        navigationSuiteItems = {
            Tab.entries.forEach { t ->
                val selected = isSelected(t)
                item(
                    selected = selected,
                    onClick = { go(t) },
                    icon = { Icon(t.icon, contentDescription = null) },
                    label = { Text(t.label, fontWeight = if (selected) FontWeight.ExtraBold else FontWeight.Medium, maxLines = 1) },
                )
            }
        },
        layoutType = layout,
        containerColor = MaterialTheme.colorScheme.background,
    ) {
        Box(Modifier.fillMaxSize()) { Destinations(nav, user) }
    }
}

@Composable
private fun Destinations(nav: NavHostController, user: UserInfo) {
    val openAlerts = { nav.navigate(AlertsRoute) { launchSingleTop = true } }
    val openAccount = { id: String -> nav.navigate(CustomerRoute(id)) }
    NavHost(nav, startDestination = HomeRoute) {
        composable<HomeRoute> {
            HomeScreen(
                user = user,
                onAlerts = openAlerts,
                onSetup = { nav.navigate(RouterRoute(it)) },
                onRouter = { nav.navigate(RouterRoute(it)) },
                onExpiring = { nav.navigate(CustomersRoute) { launchSingleTop = true } },
                onNewDevices = { nav.navigate(NewDevicesRoute()) },
                onUnmatchedPayments = { nav.navigate(PaymentsRoute(unmatched = true)) },
            )
        }
        composable<NetworkRoute> {
            NetworkScreen(
                user = user,
                onAlerts = openAlerts,
                onOpenCustomer = openAccount,
                onCreateCustomer = { routerId, mac -> nav.navigate(AddCustomerRoute(routerId, mac)) },
            )
        }
        composable<CustomersRoute> {
            CustomersScreen(
                user = user,
                onAlerts = openAlerts,
                onOpen = openAccount,
                onAdd = { nav.navigate(AddCustomerRoute()) },
                onApartments = { nav.navigate(ApartmentsRoute) },
            )
        }
        composable<PaymentsRoute> { e ->
            val r = e.toRoute<PaymentsRoute>()
            PaymentsScreen(user, onAlerts = openAlerts, initialFilter = if (r.unmatched) PaymentFilter.Unmatched else PaymentFilter.All)
        }
        composable<MoreRoute> {
            MoreScreen(
                user = user,
                onAlerts = openAlerts,
                onSetupChecks = { nav.navigate(SetupRoute(it)) },
                onRouterSetup = { nav.navigate(RouterRoute(it)) },
                onRouters = { nav.navigate(RoutersRoute) },
                onApartments = { nav.navigate(ApartmentsRoute) },
                onAlertList = { nav.navigate(AlertsRoute) },
                onSettings = { nav.navigate(SettingsRoute) },
            )
        }
        composable<CustomerRoute> { e ->
            CustomerDetailScreen(
                id = e.toRoute<CustomerRoute>().id,
                user = user,
                onBack = { nav.popBackStack() },
                onOpenOther = openAccount,
            )
        }
        composable<AddCustomerRoute> { e ->
            val r = e.toRoute<AddCustomerRoute>()
            AddCustomerScreen(
                routerId = r.routerId,
                mac = r.mac,
                onBack = { nav.popBackStack() },
                onOpenAccount = openAccount,
            )
        }
        composable<RoutersRoute> {
            RoutersScreen(
                user = user,
                onBack = { nav.popBackStack() },
                onOpen = { nav.navigate(RouterRoute(it)) },
                onAdd = { nav.navigate(AddRouterRoute) },
            )
        }
        composable<RouterRoute> { e ->
            val id = e.toRoute<RouterRoute>().id
            RouterDetailScreen(
                id = id,
                user = user,
                onBack = { nav.popBackStack() },
                onNewDevices = { nav.navigate(NewDevicesRoute(it)) },
                onOpenAccount = openAccount,
                onChecks = { nav.navigate(SetupRoute(it)) },
                onEdit = { nav.navigate(EditRouterRoute(it)) },
            )
        }
        composable<AddRouterRoute> {
            AddRouterScreen(onBack = { nav.popBackStack() }, onOpenRouter = { nav.navigate(RouterRoute(it)) })
        }
        composable<SetupRoute> { e ->
            val id = e.toRoute<SetupRoute>().id
            SetupScreen(id, user, onBack = { nav.popBackStack() })
        }
        composable<EditRouterRoute> { e ->
            EditRouterScreen(e.toRoute<EditRouterRoute>().id, onBack = { nav.popBackStack() })
        }
        composable<NewDevicesRoute> { e ->
            NewDevicesScreen(
                siteId = e.toRoute<NewDevicesRoute>().routerId,
                user = user,
                onBack = { nav.popBackStack() },
                onOpenAccount = openAccount,
            )
        }
        composable<ApartmentsRoute> {
            ApartmentsScreen(
                user = user,
                onBack = { nav.popBackStack() },
                onOpen = { nav.navigate(ApartmentRoute(it)) },
                onAdd = { nav.navigate(ApartmentFormRoute()) },
            )
        }
        composable<ApartmentRoute> { e ->
            ApartmentDetailScreen(
                id = e.toRoute<ApartmentRoute>().id,
                user = user,
                onBack = { nav.popBackStack() },
                onEdit = { nav.navigate(ApartmentFormRoute(it)) },
                onOpenAccount = openAccount,
                onOpenRouter = { nav.navigate(RouterRoute(it)) },
            )
        }
        composable<ApartmentFormRoute> { e ->
            ApartmentFormScreen(
                id = e.toRoute<ApartmentFormRoute>().id,
                onBack = { nav.popBackStack() },
                onSaved = { id ->
                    // Land on the apartment page; the form is not kept in the back stack.
                    nav.navigate(ApartmentRoute(id)) { popUpTo<ApartmentFormRoute> { inclusive = true } }
                },
            )
        }
        composable<AlertsRoute> { AlertsScreen(user, onBack = { nav.popBackStack() }) }
        composable<SettingsRoute> { SettingsScreen(onBack = { nav.popBackStack() }) }
    }
}
