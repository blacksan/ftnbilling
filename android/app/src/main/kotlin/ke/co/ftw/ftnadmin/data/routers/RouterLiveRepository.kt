package ke.co.ftw.ftnadmin.data.routers

import ke.co.ftw.ftnadmin.core.network.ApiClient
import ke.co.ftw.ftnadmin.data.putIfNotBlank
import ke.co.ftw.ftnadmin.domain.Site
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put

/**
 * Live router data (API v4). Nothing is cached here: each screen polls and the server caches for 5 s.
 * Paths are the `/routers` aliases; the server also serves them under `/sites`.
 */
class RouterLiveRepository(private val api: ApiClient) {
    suspend fun live(routerId: String): RouterLive = api.get("routers", routerId, "live")

    suspend fun interfaces(routerId: String): List<NetInterface> =
        api.get<InterfacesResponse>("routers", routerId, "interfaces").items

    /** Every physical port with its role; the server reads the router once per refresh (cached 4 s). */
    suspend fun ports(routerId: String): PortsResponse = api.get("routers", routerId, "ports")

    /**
     * Enable/disable and change the role of one port (operator; owner for the admin LAN and the OLT uplink).
     * [confirmName] is the typed router name the server asks for before risky changes.
     */
    suspend fun changePort(routerId: String, port: String, role: String? = null, disabled: Boolean? = null, confirmName: String? = null): PortChangeResponse =
        api.patch(
            "routers", routerId, "ports", port,
            body = buildJsonObject {
                role?.let { put("role", it) }
                disabled?.let { put("disabled", it) }
                if (!confirmName.isNullOrBlank()) put("confirm_name", confirmName)
            },
        )

    /** [kind] is `all`, `flats` or `unregistered`; [online] filters to connected devices. */
    suspend fun clients(routerId: String, query: String? = null, kind: String? = null, online: Boolean? = null): ClientsResponse =
        api.get(
            "routers", routerId, "clients",
            query = mapOf("q" to query?.trim(), "kind" to kind, "online" to online?.toString()),
        )

    /** Fleet totals. Pass a router id to scope them to one router. */
    suspend fun overview(routerId: String? = null): Overview = api.get("overview", query = mapOf("router" to routerId))

    /** Owner only. The response carries the one-time join line. Blank fields take the server's defaults. */
    suspend fun createRouter(r: RouterInput): CreatedRouter = api.post("routers", body = routerBody(r, patch = false))

    /**
     * Owner only. Only the fields present change: the form sends every field so a blank clears it, except the
     * write-only password, which is sent only when typed (or cleared with `pppoe_pass_clear`).
     */
    suspend fun updateRouter(id: String, r: RouterInput): Site = api.patch("routers", id, body = routerBody(r, patch = true))

    private fun routerBody(r: RouterInput, patch: Boolean) = buildJsonObject {
        fun str(key: String, v: String) {
            if (patch) put(key, v.trim()) else putIfNotBlank(key, v.trim())
        }
        put("name", r.name.trim())
        str("wan_interface", r.wanInterface)
        put("wan_mode", r.wanMode)
        str("wan_address", r.wanAddress)
        str("wan_gateway", r.wanGateway)
        str("wan_dns", r.wanDns)
        str("pppoe_user", r.pppoeUser)
        if (r.pppoePass.isNotEmpty()) put("pppoe_pass", r.pppoePass)
        if (r.pppoePassClear) put("pppoe_pass_clear", true)
        str("lan_ports", r.lanPorts)
        str("lan_subnet", r.lanSubnet)
        str("olt_interface", r.oltInterface)
        r.vlanId?.let { put("vlan_id", it) }
        str("customer_subnet", r.customerSubnet)
        if (patch || r.customerPorts.isNotEmpty()) put("customer_ports", JsonArray(r.customerPorts.map { JsonPrimitive(it) }))
        r.wanCapMbps?.let { put("wan_cap_mbps", it) }
        str("support_phone", r.supportPhone)
        if (r.confirm) put("confirm", true)
        if (r.confirmName.isNotBlank()) put("confirm_name", r.confirmName.trim())
    }
}
