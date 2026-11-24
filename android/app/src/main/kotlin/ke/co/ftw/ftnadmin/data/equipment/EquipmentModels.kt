package ke.co.ftw.ftnadmin.data.equipment

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable

/** An apartment as the equipment tree names it. */
@Serializable
data class TreeApartment(val id: String = "", val name: String = "")

/** The house an ONU serves (null when the ONU is not linked to an account). */
@Serializable
data class TreeHouse(
    @SerialName("account_id") val accountId: String = "",
    val code: String = "",
    val unit: String = "",
    @SerialName("apartment_id") val apartmentId: String = "",
    val apartment: String = "",
    val customer: String = "",
)

/** TR-069 link of an ONU (null when the ONU never informed). */
@Serializable
data class TreeTr069(
    val linked: Boolean = false,
    @SerialName("last_inform") val lastInform: String? = null,
    @SerialName("held_back") val heldBack: Boolean = false,
)

@Serializable
data class TreeOnu(
    val id: String = "",
    val serial: String = "",
    @SerialName("gpon_serial") val gponSerial: String = "",
    val model: String = "",
    val mac: String = "",
    val status: String = "",
    @SerialName("olt_id") val oltId: String = "",
    @SerialName("pon_port") val ponPort: Int = 0,
    val house: TreeHouse? = null,
    val online: Boolean = false,
    @SerialName("last_seen") val lastSeen: String? = null,
    val tr069: TreeTr069? = null,
    val warnings: List<String> = emptyList(),
) {
    val title: String get() = serial.ifBlank { mac.ifBlank { "(ONU)" } }
}

@Serializable
data class TreePon(
    val port: Int = 0,
    val apartments: List<TreeApartment> = emptyList(),
    val onus: List<TreeOnu> = emptyList(),
) {
    val apartmentText: String get() = if (apartments.isEmpty()) "No apartments assigned" else apartments.joinToString(", ") { it.name }
}

@Serializable
data class TreeOlt(
    val id: String = "",
    val name: String = "",
    val model: String = "",
    val serial: String = "",
    val mac: String = "",
    val firmware: String = "",
    val status: String = "",
    @SerialName("router_id") val routerId: String = "",
    @SerialName("router_port") val routerPort: String = "",
    @SerialName("pon_ports") val ponPorts: Int = 0,
    @SerialName("mgmt_ip") val mgmtIp: String = "",
    @SerialName("mgmt_user") val mgmtUser: String = "",
    @SerialName("has_mgmt_pass") val hasMgmtPass: Boolean = false,
    @SerialName("onu_count") val onuCount: Int = 0,
    @SerialName("online_count") val onlineCount: Int = 0,
    @SerialName("warning_count") val warningCount: Int = 0,
    val pons: List<TreePon> = emptyList(),
) {
    val summary: String
        get() = "$onuCount ONUs, $onlineCount online" + if (warningCount > 0) ", $warningCount to check" else ""
}

@Serializable
data class TreeRouter(
    val id: String = "",
    val name: String = "",
    val apartments: List<TreeApartment> = emptyList(),
    val olts: List<TreeOlt> = emptyList(),
    @SerialName("onu_count") val onuCount: Int = 0,
    @SerialName("online_count") val onlineCount: Int = 0,
)

/** GET /equipment/tree: routers (physical and logical), with the OLTs and ONUs below, and what is not placed yet. */
@Serializable
data class EquipmentTree(
    val routers: List<TreeRouter> = emptyList(),
    @SerialName("unplaced_olts") val unplacedOlts: List<TreeOlt> = emptyList(),
    @SerialName("unplaced_onus") val unplacedOnus: List<TreeOnu> = emptyList(),
)

/** An OLT of the register (GET /olts, GET /olts/{id}). The management password is never sent: only whether one is stored. */
@Serializable
data class Olt(
    val id: String = "",
    val name: String = "",
    val model: String = "",
    val serial: String = "",
    val mac: String = "",
    val firmware: String = "",
    val notes: String = "",
    val status: String = "",
    @SerialName("site_id") val siteId: String = "",
    @SerialName("router_port") val routerPort: String = "",
    @SerialName("pon_ports") val ponPorts: Int = 0,
    /** Port number (as text) to the ids of the apartments it serves. */
    @SerialName("pon_apartments") val ponApartments: Map<String, List<String>> = emptyMap(),
    @SerialName("mgmt_ip") val mgmtIp: String = "",
    @SerialName("mgmt_user") val mgmtUser: String = "",
    @SerialName("has_mgmt_pass") val hasMgmtPass: Boolean = false,
)

/** What the OLT form sends. [mgmtPass] is write-only: blank keeps the stored one unless [clearMgmtPass]. */
data class OltInput(
    val name: String,
    val mac: String,
    val model: String = "",
    val serial: String = "",
    val firmware: String = "",
    val notes: String = "",
    val siteId: String = "",
    val routerPort: String = "",
    val ponPorts: Int = 0,
    /** Null leaves the stored mapping alone (omitted on the wire); an empty map clears it. */
    val ponApartments: Map<Int, List<String>>? = null,
    val mgmtIp: String = "",
    val mgmtUser: String = "",
    val mgmtPass: String = "",
    val clearMgmtPass: Boolean = false,
    val status: String = "",
)

/** The answer to PUT /equipment/{id}/placement: the ONU with its warnings (a PON port that does not serve its house). */
@Serializable
data class OnuPlacement(
    val id: String = "",
    val serial: String = "",
    @SerialName("olt_id") val oltId: String = "",
    @SerialName("olt_name") val oltName: String = "",
    @SerialName("pon_port") val ponPort: Int = 0,
    val warnings: List<String> = emptyList(),
)
