package ke.co.ftw.ftnadmin.data.routers

import ke.co.ftw.ftnadmin.core.network.ApiClient
import ke.co.ftw.ftnadmin.data.putIfNotBlank
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

    /** [kind] is `all`, `flats` or `unregistered`; [online] filters to connected devices. */
    suspend fun clients(routerId: String, query: String? = null, kind: String? = null, online: Boolean? = null): ClientsResponse =
        api.get(
            "routers", routerId, "clients",
            query = mapOf("q" to query?.trim(), "kind" to kind, "online" to online?.toString()),
        )

    /** Fleet totals. Pass a router id to scope them to one router. */
    suspend fun overview(routerId: String? = null): Overview = api.get("overview", query = mapOf("router" to routerId))

    /** Owner only. The response carries the one-time join command. */
    suspend fun createRouter(r: NewRouter): CreatedRouter = api.post(
        "routers",
        body = buildJsonObject {
            put("name", r.name.trim())
            put("serving_customers", r.servingCustomers)
            putIfNotBlank("wan_interface", r.wanInterface.trim())
            putIfNotBlank("olt_interface", r.oltInterface.trim())
            r.vlanId?.let { put("vlan_id", it) }
            putIfNotBlank("customer_subnet", r.customerSubnet.trim())
            r.wanCapMbps?.let { put("wan_cap_mbps", it) }
            putIfNotBlank("support_phone", r.supportPhone.trim())
        },
    )
}
