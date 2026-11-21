package ke.co.ftw.ftnadmin.data.sites

import ke.co.ftw.ftnadmin.core.network.ApiClient
import ke.co.ftw.ftnadmin.data.putIfNotBlank
import ke.co.ftw.ftnadmin.domain.Account
import ke.co.ftw.ftnadmin.domain.ItemList
import ke.co.ftw.ftnadmin.domain.NewDevice
import ke.co.ftw.ftnadmin.domain.PendingRegistration
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
            put("router_id", siteId)
            put("mac", mac)
            put("account_id", accountId)
        },
    )

    suspend fun create(
        siteId: String,
        apartmentId: String,
        mac: String,
        unit: String,
        tenant: String?,
        phone: String?,
        packageId: String?,
    ): Account =
        api.post(
            "unregistered", "create",
            body = buildJsonObject {
                put("site_id", siteId)
                put("router_id", siteId)
                put("apartment_id", apartmentId)
                put("mac", mac)
                put("unit", unit)
                putIfNotBlank("tenant", tenant)
                putIfNotBlank("phone", phone)
                putIfNotBlank("package_id", packageId)
            },
        )

    suspend fun dismiss(siteId: String, mac: String) = api.send(
        "POST", "unregistered", "dismiss",
        body = buildJsonObject {
            put("site_id", siteId)
            put("router_id", siteId)
            put("mac", mac)
        },
    )

    /**
     * "Move customer here": the MAC belongs to a customer of another apartment. The customer follows the device to the
     * router that sees it (and to [apartmentId] when that router serves several; empty keeps the only choice).
     */
    suspend fun moveCustomerHere(siteId: String, mac: String, apartmentId: String?): Account = api.post(
        "unregistered", "move",
        body = buildJsonObject {
            put("site_id", siteId)
            put("router_id", siteId)
            put("mac", mac)
            putIfNotBlank("apartment_id", apartmentId)
        },
    )

    /** Self-registrations from the captive page that wait for the owner (GET /registrations). */
    suspend fun registrations(siteId: String? = null): List<PendingRegistration> =
        api.get<ItemList<PendingRegistration>>("registrations", query = mapOf("router" to siteId)).items

    /** Creates the account, the customer and the device. [mac] is the device the owner picked in the list. */
    suspend fun approveRegistration(id: String, mac: String) = api.send(
        "POST", "registrations", id, "approve",
        body = buildJsonObject { put("mac", mac) },
    )

    /**
     * Rejects every waiting registration of one router ([siteId]), or the ones that name no router yet when [siteId] is
     * blank; the server erases their details.
     */
    suspend fun rejectAllRegistrations(siteId: String) = api.send(
        "POST", "registrations", "reject-all",
        body = buildJsonObject {
            if (siteId.isBlank()) put("unassigned", true) else put("site_id", siteId)
        },
    )

    /** Declines the registration; the server erases the person's details. */
    suspend fun rejectRegistration(id: String) = api.send("POST", "registrations", id, "reject", body = buildJsonObject {})
}
