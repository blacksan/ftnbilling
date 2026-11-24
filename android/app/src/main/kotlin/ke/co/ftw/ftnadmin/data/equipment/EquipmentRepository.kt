package ke.co.ftw.ftnadmin.data.equipment

import ke.co.ftw.ftnadmin.core.network.ApiClient
import ke.co.ftw.ftnadmin.data.onu.RegisterItem
import ke.co.ftw.ftnadmin.data.putIfNotBlank
import ke.co.ftw.ftnadmin.domain.ItemList
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put

/** The equipment hierarchy: router > OLT > PON port > ONU > house. No router is written to by any of these calls. */
class EquipmentRepository(private val api: ApiClient) {
    suspend fun tree(): EquipmentTree = api.get("equipment", "tree")

    /** Every item of the equipment register, any kind (the Other tab keeps what is not an OLT or an ONU). */
    suspend fun items(): List<RegisterItem> = api.get<ItemList<RegisterItem>>("equipment").items

    suspend fun olts(siteId: String? = null): List<Olt> =
        api.get<ItemList<Olt>>("olts", query = mapOf("site" to siteId)).items

    suspend fun olt(id: String): Olt = api.get("olts", id)

    /** What the server last read from the OLT: online ONUs, optics, and serials found on the PON that are not in inventory. */
    suspend fun oltStatus(id: String): OltStatus = api.get("olts", id, "status")

    /** Adds an OLT, or changes [id]. A 400 on `mac` means the MAC is an account's or another device's. */
    suspend fun saveOlt(id: String?, input: OltInput): Olt =
        if (id == null) api.post("olts", body = oltBody(input)) else api.put("olts", id, body = oltBody(input))

    /** Puts an ONU under [oltId]'s PON [port]; a blank [oltId] takes it out. The answer carries the warnings. */
    suspend fun setPlacement(onuId: String, oltId: String, port: Int): OnuPlacement = api.put(
        "equipment", onuId, "placement",
        body = buildJsonObject {
            put("olt_id", oltId)
            put("pon_port", port)
        },
    )

    companion object {
        /** The request body. The password is sent only when typed or when clearing; never echoed anywhere else. */
        internal fun oltBody(i: OltInput): JsonObject = buildJsonObject {
            put("name", i.name.trim())
            put("mac", i.mac.trim())
            put("model", i.model.trim())
            put("serial", i.serial.trim())
            put("firmware", i.firmware.trim())
            put("notes", i.notes.trim())
            put("site_id", i.siteId)
            put("router_port", i.routerPort.trim())
            put("pon_ports", i.ponPorts)
            i.ponApartments?.let { m ->
                put(
                    "pon_apartments",
                    JsonObject(m.entries.sortedBy { it.key }.associate { (k, v) -> k.toString() to JsonArray(v.map { JsonPrimitive(it) }) }),
                )
            }
            put("mgmt_ip", i.mgmtIp.trim())
            put("mgmt_user", i.mgmtUser.trim())
            if (i.mgmtPass.isNotEmpty()) put("mgmt_pass", i.mgmtPass)
            if (i.clearMgmtPass) put("clear_mgmt_pass", true)
            putIfNotBlank("status", i.status)
        }
    }
}
