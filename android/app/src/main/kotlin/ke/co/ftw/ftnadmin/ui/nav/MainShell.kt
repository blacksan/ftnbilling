package ke.co.ftw.ftnadmin.ui.nav

import androidx.annotation.StringRes
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.AccountBox
import androidx.compose.material.icons.filled.Home
import androidx.compose.material.icons.filled.Menu
import androidx.compose.material.icons.filled.Person
import androidx.compose.material.icons.filled.Lan
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
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.style.TextOverflow
import androidx.navigation.NavDestination.Companion.hasRoute
import androidx.navigation.NavDestination.Companion.hierarchy
import androidx.navigation.NavGraph.Companion.findStartDestination
import androidx.navigation.NavHostController
import androidx.navigation.compose.NavHost
import androidx.navigation.compose.composable
import androidx.navigation.compose.currentBackStackEntryAsState
import androidx.navigation.compose.rememberNavController
import androidx.navigation.toRoute
import ke.co.ftw.ftnadmin.R
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
import ke.co.ftw.ftnadmin.ui.routers.setup.SetupScreen
import ke.co.ftw.ftnadmin.ui.settings.SettingsScreen
import kotlin.reflect.KClass

private enum class Tab(val route: Any, val type: KClass<*>, @StringRes val label: Int, val icon: ImageVector) {
    Home(HomeRoute, HomeRoute::class, R.string.tab_home, Icons.Default.Home),
    Network(NetworkRoute, NetworkRoute::class, R.string.tab_network, Icons.Default.Lan),
    Customers(CustomersRoute, CustomersRoute::class, R.string.tab_customers, Icons.Default.Person),
    Payments(PaymentsRoute(), PaymentsRoute::class, R.string.tab_payments, Icons.Default.AccountBox),
    More(MoreRoute, MoreRoute::class, R.string.tab_more, Icons.Default.Menu),
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
                    // One weight for every state, so the label does not change width when the tab is selected; the indicator shows selection.
                    label = { Text(stringResource(t.label), maxLines = 1, overflow = TextOverflow.Ellipsis) },
                )
            }
        },
        layoutType = layout,
        containerColor = MaterialTheme.colorScheme.background,
    ) {
        // The navigation bar takes the bottom inset itself; the rail does not, so content keeps clear of the gesture bar.
        val bottomInset = if (layout == NavigationSuiteType.NavigationRail) Modifier.navigationBarsPadding() else Modifier
        Box(Modifier.fillMaxSize().then(bottomInset)) { Destinations(nav, user) }
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
                onChangeMac = { id -> nav.navigate(CustomerRoute(id, editMac = true)) },
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
                onEquipment = { nav.navigate(EquipmentRoute()) },
            )
        }
        composable<EquipmentRoute> { e ->
            ke.co.ftw.ftnadmin.ui.equipment.EquipmentTabsScreen(
                user = user,
                initialTab = ke.co.ftw.ftnadmin.ui.equipment.EquipmentTab.fromKey(e.toRoute<EquipmentRoute>().tab),
                onBack = { nav.popBackStack() },
                onOpenRouter = { nav.navigate(RouterRoute(it)) },
                onAddRouter = { nav.navigate(AddRouterRoute) },
                onAddOlt = { nav.navigate(OltFormRoute(routerId = it)) },
                onEditOlt = { nav.navigate(OltFormRoute(id = it)) },
                onAddOnu = { nav.navigate(OnuRegisterRoute) },
                onOpenAccount = openAccount,
                onTree = { nav.navigate(EquipmentTreeRoute) },
            )
        }
        composable<EquipmentTreeRoute> {
            ke.co.ftw.ftnadmin.ui.equipment.EquipmentScreen(
                user = user,
                onBack = { nav.popBackStack() },
                onOpenAccount = openAccount,
                onAddOlt = { nav.navigate(OltFormRoute(routerId = it)) },
                onEditOlt = { nav.navigate(OltFormRoute(id = it)) },
            )
        }
        composable<OltFormRoute> { e ->
            val r = e.toRoute<OltFormRoute>()
            ke.co.ftw.ftnadmin.ui.equipment.OltFormScreen(
                oltId = r.id,
                initialSiteId = r.routerId,
                onBack = { nav.popBackStack() },
                onSaved = { nav.popBackStack() },
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
                onEquipment = { nav.navigate(EquipmentRoute()) },
                onApartments = { nav.navigate(ApartmentsRoute) },
                onAlertList = { nav.navigate(AlertsRoute) },
                onSettings = { nav.navigate(SettingsRoute) },
            )
        }
        composable<CustomerRoute> { e ->
            CustomerDetailScreen(
                id = e.toRoute<CustomerRoute>().id,
                openEditMac = e.toRoute<CustomerRoute>().editMac,
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
        composable<RouterRoute> { e ->
            val id = e.toRoute<RouterRoute>().id
            RouterDetailScreen(
                id = id,
                user = user,
                onBack = { nav.popBackStack() },
                onNewDevices = { nav.navigate(NewDevicesRoute(it)) },
                onOpenAccount = openAccount,
                onChecks = { nav.navigate(SetupRoute(it)) },
                onPorts = { id2, name -> nav.navigate(PortsRoute(id2, name)) },
                onEdit = { nav.navigate(EditRouterRoute(it)) },
                onOnus = { nav.navigate(OnusRoute(it)) },
            )
        }
        composable<OnusRoute> { e ->
            ke.co.ftw.ftnadmin.ui.onu.OnusScreen(
                siteId = e.toRoute<OnusRoute>().routerId,
                user = user,
                onBack = { nav.popBackStack() },
                onOpenAccount = openAccount,
                onRegister = { nav.navigate(OnuRegisterRoute) },
            )
        }
        composable<OnuRegisterRoute> {
            ke.co.ftw.ftnadmin.ui.onu.RegisterScreen(user = user, onBack = { nav.popBackStack() })
        }
        composable<AddRouterRoute> {
            AddRouterScreen(onBack = { nav.popBackStack() }, onOpenRouter = { nav.navigate(RouterRoute(it)) })
        }
        composable<SetupRoute> { e ->
            val id = e.toRoute<SetupRoute>().id
            SetupScreen(id, user, onBack = { nav.popBackStack() })
        }
        composable<PortsRoute> { e ->
            val r = e.toRoute<PortsRoute>()
            ke.co.ftw.ftnadmin.ui.routers.ports.PortsScreen(r.id, r.name, onBack = { nav.popBackStack() })
        }
        composable<EditRouterRoute> { e ->
            val routerId = e.toRoute<EditRouterRoute>().id
            EditRouterScreen(routerId, onBack = { nav.popBackStack() }, onOpenPorts = { name -> nav.navigate(PortsRoute(routerId, name)) })
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
