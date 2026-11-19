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

@Serializable data object RoutersRoute

@Serializable data class RouterRoute(val id: String)

@Serializable data object AddRouterRoute

@Serializable data class SetupRoute(val id: String)

@Serializable data class PortsRoute(val id: String, val name: String = "")

@Serializable data class EditRouterRoute(val id: String)

@Serializable data class NewDevicesRoute(val routerId: String? = null)

@Serializable data object AlertsRoute

@Serializable data object SettingsRoute
