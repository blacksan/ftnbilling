package ke.co.ftw.ftnadmin.data.accounts

import ke.co.ftw.ftnadmin.core.network.ApiClient
import ke.co.ftw.ftnadmin.data.putIfNotBlank
import ke.co.ftw.ftnadmin.domain.Account
import ke.co.ftw.ftnadmin.domain.ItemList
import ke.co.ftw.ftnadmin.domain.Page
import ke.co.ftw.ftnadmin.domain.ServicePackage
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put

class AccountsRepository(private val api: ApiClient) {
    suspend fun list(
        siteId: String? = null,
        state: String? = null,
        query: String? = null,
        cursor: String? = null,
        limit: Int = 50,
    ): Page<Account> = api.get(
        "accounts",
        query = mapOf("router" to siteId, "state" to state, "q" to query, "cursor" to cursor, "limit" to limit.toString()),
    )

    suspend fun get(id: String): Account = api.get("accounts", id)

    suspend fun packages(siteId: String? = null): List<ServicePackage> =
        api.get<ItemList<ServicePackage>>("packages", query = mapOf("router" to siteId)).items

    /**
     * Creates a customer account in an apartment (API v6: `apartment_id` and `unit` are required) and, when [activate] is set, applies [packageId] straight away
     * (API v4 `POST /accounts`). The server tries the router once before answering, so the
     * returned account's `router` says whether it is already provisioned or queued.
     */
    suspend fun create(
        apartmentId: String,
        routerId: String?,
        unit: String,
        tenant: String?,
        phone: String?,
        mac: String,
        packageId: String?,
        activate: Boolean,
        note: String? = null,
        /** The operator said yes to a device that never asked for an address (after a 409 confirm_required). */
        confirmNoAddress: Boolean = false,
    ): Account = api.post(
        "accounts",
        body = buildJsonObject {
            put("apartment_id", apartmentId)
            // Empty router: the server picks the apartment's only router.
            if (!routerId.isNullOrBlank()) put("router_id", routerId)
            put("unit", unit.trim())
            putIfNotBlank("tenant", tenant?.trim())
            putIfNotBlank("phone", phone?.trim())
            put("mac", mac)
            putIfNotBlank("package_id", packageId)
            put("activate", activate)
            putIfNotBlank("note", note)
            if (confirmNoAddress) put("confirm_no_address", true)
        },
    )

    /** Saves the WhatsApp number; [samePhone] makes it the phone number (the server keeps the box ticked and follows the phone). */
    suspend fun setWhatsApp(id: String, whatsapp: String?, samePhone: Boolean): Account = api.patch(
        "accounts", id,
        body = buildJsonObject {
            put("wa_same_phone", samePhone)
            if (!samePhone) put("whatsapp", whatsapp?.trim().orEmpty())
        },
    )

    /**
     * Saves the Details form: tenant, phone, WhatsApp and notes (`PATCH /accounts/{id}`). The house number is deliberately not sent:
     * it changes only through Move. With [samePhone] the WhatsApp number follows the phone and [whatsapp] is not sent.
     */
    suspend fun updateDetails(id: String, tenant: String, phone: String, whatsapp: String?, samePhone: Boolean, notes: String): Account = api.patch(
        "accounts", id,
        body = buildJsonObject {
            put("tenant", tenant.trim())
            put("phone", phone.trim())
            put("notes", notes.trim())
            put("wa_same_phone", samePhone)
            if (!samePhone) put("whatsapp", whatsapp?.trim().orEmpty())
        },
    )

    suspend fun suspendAccount(id: String): Account = api.post("accounts", id, "suspend")

    suspend fun unsuspendAccount(id: String): Account = api.post("accounts", id, "unsuspend")

    /** Activates or extends: the server applies the package from now, or on top of the current expiry. */
    suspend fun extend(id: String, packageId: String, note: String? = null, force: Boolean = false): Account =
        api.post(
            "accounts", id, "extend",
            body = buildJsonObject {
                put("package_id", packageId)
                putIfNotBlank("note", note)
                if (force) put("force", true)
            },
        )

    suspend fun changePackage(id: String, packageId: String, keepTime: Boolean): Account = api.post(
        "accounts", id, "package",
        body = buildJsonObject {
            put("package_id", packageId)
            put("keep_time", keepTime)
        },
    )

    /** [mac] must already be normalised (see MacAddress). A 409 `mac_in_use` carries the owner. */
    suspend fun changeMac(id: String, mac: String): Account =
        api.put("accounts", id, "mac", body = buildJsonObject { put("mac", mac) })

    suspend fun reconcile(id: String) = api.send("POST", "accounts", id, "reconcile")
}
