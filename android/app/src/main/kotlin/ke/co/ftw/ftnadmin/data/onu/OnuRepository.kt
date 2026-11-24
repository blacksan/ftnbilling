package ke.co.ftw.ftnadmin.data.onu

import ke.co.ftw.ftnadmin.core.network.ApiClient
import ke.co.ftw.ftnadmin.data.putIfNotBlank
import ke.co.ftw.ftnadmin.domain.ItemList
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put

/** ONU management (TR-069) and the ONU register. A Wi-Fi password is only ever sent, never read back. */
class OnuRepository(private val api: ApiClient) {
    suspend fun ofAccount(accountId: String): AccountOnu = api.get("accounts", accountId, "onu")

    /** [pass5] and [ssid5] count only when [same] is false. */
    suspend fun setWifi(
        accountId: String,
        ssid: String,
        pass: String,
        same: Boolean,
        ssid5: String = "",
        pass5: String = "",
    ): OnuTaskReply = api.put(
        "accounts", accountId, "wifi",
        body = buildJsonObject {
            put("ssid", ssid)
            put("password", pass)
            put("same_for_both", same)
            if (!same) {
                put("ssid_5g", ssid5)
                put("password_5g", pass5)
            }
        },
    )

    suspend fun reboot(accountId: String): OnuTaskReply = api.post("accounts", accountId, "onu", "reboot")

    suspend fun refresh(accountId: String): OnuTaskReply = api.post("accounts", accountId, "onu", "refresh")

    /** "It works": the newest accepted Wi-Fi change becomes confirmed, and the ONU model remembers what took. */
    suspend fun confirmWifi(accountId: String): OnuTaskReply = api.post("accounts", accountId, "onu", "confirm")

    /** "It didn't work": the change is queued again another way. */
    suspend fun rejectWifi(accountId: String): OnuTaskReply = api.post("accounts", accountId, "onu", "reject")

    /** Cancels a queued task before it is sent. */
    suspend fun cancelTask(taskId: String): kotlinx.serialization.json.JsonObject = api.post("onu-tasks", taskId, "cancel")

    /** Answer to "ONU checked in from an unexpected place": bind it to where it came from. */
    suspend fun approve(onuId: String): Onu = api.post("onus", onuId, "approve")

    /** The other answer: that place is turned away for this serial. */
    suspend fun block(onuId: String): Onu = api.post("onus", onuId, "block")

    suspend fun unblock(onuId: String): Onu = api.post("onus", onuId, "unblock")

    /** Owner only: trust the management MAC a waiting ONU checked in from. */
    suspend fun trust(onuId: String): Onu = api.post("onus", onuId, "trust")

    /** Serials that tried to check in and are not in the ONU inventory. */
    suspend fun unknown(siteId: String? = null): List<UnknownOnu> =
        api.get<ItemList<UnknownOnu>>("onus", "unknown", query = if (siteId == null) emptyMap() else mapOf("site_id" to siteId)).items

    suspend fun forRouter(siteId: String): List<Onu> = api.get<ItemList<Onu>>("sites", siteId, "onus").items

    suspend fun link(onuId: String, accountId: String): Onu =
        api.post("onus", onuId, "link", body = buildJsonObject { put("account_id", accountId) })

    suspend fun unlink(onuId: String): Onu = api.post("onus", onuId, "unlink")

    /** Adds the ONU's serial and one of the MACs it reported to the register. */
    suspend fun addToRegister(onuId: String, mac: String): RegisterItem =
        api.post("onus", onuId, "inventory", body = buildJsonObject { put("mac", mac) })

    suspend fun register(): List<RegisterItem> =
        api.get<ItemList<RegisterItem>>("equipment", query = mapOf("kind" to "ont")).items

    /** [mac] must already be normalised (see MacAddress). A 409 means the serial or MAC is already registered. */
    suspend fun addRegisterItem(
        serial: String,
        gponSerial: String?,
        mac: String,
        model: String?,
        /** The OLT the ONU hangs off and its PON port; both blank/zero leave it unplaced. */
        oltId: String? = null,
        ponPort: Int = 0,
    ): RegisterItem = api.post(
        "equipment",
        body = buildJsonObject {
            put("kind", "ont")
            put("serial", serial.trim())
            putIfNotBlank("gpon_serial", gponSerial?.trim())
            put("mac", mac)
            putIfNotBlank("model", model?.trim())
            if (!oltId.isNullOrBlank()) {
                put("olt_id", oltId)
                put("pon_port", ponPort)
            }
        },
    )

    /**
     * Swaps the house's ONU for one of the register. The old one is unlinked at once and a factory reset is queued for
     * its next check-in; it goes back with [oldStatus] (the server's default is returned). The app has no screen for it yet.
     */
    suspend fun swap(accountId: String, equipmentId: String, oldStatus: String): kotlinx.serialization.json.JsonObject =
        api.post(
            "accounts", accountId, "onu", "swap",
            body = buildJsonObject {
                put("equipment_id", equipmentId)
                put("old_status", oldStatus)
            },
        )

    /** The one-click answer to a MAC mismatch warning. */
    suspend fun useReportedMac(accountId: String, mac: String): kotlinx.serialization.json.JsonObject =
        api.post("accounts", accountId, "onu", "use-reported-mac", body = buildJsonObject { put("mac", mac) })
}
