package ke.co.ftw.ftnadmin.ui.nav

import kotlinx.serialization.Serializable

// Type-safe navigation routes (Navigation Compose with kotlinx.serialization).

@Serializable data object HomeRoute

@Serializable data object NetworkRoute

@Serializable data object CustomersRoute

@Serializable data class PaymentsRoute(val unmatched: Boolean = false)

@Serializable data object MoreRoute

@Serializable data class CustomerRoute(val id: String)

@Serializable data class AddCustomerRoute(val routerId: String? = null, val mac: String? = null)

@Serializable data class RouterRoute(val id: String)

@Serializable data object AddRouterRoute

@Serializable data class SetupRoute(val id: String)

@Serializable data class PortsRoute(val id: String, val name: String = "")

@Serializable data class EditRouterRoute(val id: String)

@Serializable data class NewDevicesRoute(val routerId: String? = null)

@Serializable data object ApartmentsRoute

@Serializable data class ApartmentRoute(val id: String)

/** Create (id = null) or edit an apartment. */
@Serializable data class ApartmentFormRoute(val id: String? = null)

@Serializable data object AlertsRoute

@Serializable data object SettingsRoute

/** The ONUs (TR-069) of one router. */
@Serializable data class OnusRoute(val routerId: String)

/**
 * Equipment: one screen with four tabs, Routers, OLT, ONU and Other. [tab] is an [ke.co.ftw.ftnadmin.ui.equipment.EquipmentTab]
 * key ("routers", "olt", "onu", "other"); anything else opens Routers. Switching tabs stays on the one back-stack entry.
 */
@Serializable data class EquipmentRoute(val tab: String = "routers")

/** The equipment hierarchy: router, OLT, PON port, ONU, house (the Tree view of Equipment). */
@Serializable data object EquipmentTreeRoute

/** Add (id = null) or edit an OLT; [routerId] preselects the router when added from a router's page. */
@Serializable data class OltFormRoute(val id: String? = null, val routerId: String? = null)

/** The ONU register: serial and WAN MAC pairs. */
@Serializable data object OnuRegisterRoute
