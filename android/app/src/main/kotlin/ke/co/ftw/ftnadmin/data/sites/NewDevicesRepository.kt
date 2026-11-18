package ke.co.ftw.ftnadmin.data.sites

import ke.co.ftw.ftnadmin.core.network.ApiClient
import ke.co.ftw.ftnadmin.data.putIfNotBlank
import ke.co.ftw.ftnadmin.domain.Account
import ke.co.ftw.ftnadmin.domain.ItemList
import ke.co.ftw.ftnadmin.domain.NewDevice
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put

/** Connected-but-unconfigured MACs (REBUILD-PLAN section H). Writes go through the server's ChangeMAC rules. */
class NewDevicesRepository(private val api: ApiClient) {
    suspend fun list(siteId: String? = null, onlineOnly: Boolean = false): List<NewDevice> =
        api.get<ItemList<NewDevice>>(
            "unregistered",
            query = mapOf("router" to siteId, "online" to if (onlineOnly) "true" else null),
        ).items

    /** Asks the server to re-read the router's leases and ARP table now (202). */
    suspend fun refresh(siteId: String) = api.send("POST", "routers", siteId, "unregistered", "refresh")

    suspend fun attach(siteId: String, mac: String, accountId: String): Account = api.post(
        "unregistered", "attach",
        body = buildJsonObject {
            put("site_id", siteId)
            put("mac", mac)
            put("account_id", accountId)
        },
    )

    suspend fun create(siteId: String, mac: String, flat: String, tenant: String?, phone: String?, packageId: String?): Account =
        api.post(
            "unregistered", "create",
            body = buildJsonObject {
                put("site_id", siteId)
                put("mac", mac)
                put("flat", flat)
                putIfNotBlank("tenant", tenant)
                putIfNotBlank("phone", phone)
                putIfNotBlank("package_id", packageId)
            },
        )

    suspend fun dismiss(siteId: String, mac: String) = api.send(
        "POST", "unregistered", "dismiss",
        body = buildJsonObject {
            put("site_id", siteId)
            put("mac", mac)
        },
    )
}
