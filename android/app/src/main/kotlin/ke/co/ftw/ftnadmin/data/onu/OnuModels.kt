package ke.co.ftw.ftnadmin.data.onu

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable

/** One Wi-Fi network as the ONU reports it. The current password is never sent by the server. */
@Serializable
data class OnuWifi(
    val band: String = "",
    val ssid: String = "",
    val enabled: Boolean = true,
)

/** The saved Wi-Fi choice, without its password. */
@Serializable
data class OnuDesired(
    val set: Boolean = false,
    val same: Boolean = true,
    val ssid: String = "",
    val ssid5: String = "",
    @SerialName("has_password") val hasPassword: Boolean = false,
    @SerialName("changed_on_device") val changedOnDevice: Boolean = false,
)

/**
 * A request to the ONU. [status] is queued, sending, accepted, confirmed, failed, expired or done.
 * Accepted only means the box said yes: the owner confirms a Wi-Fi change with "It works" ([canConfirm]).
 */
@Serializable
data class OnuTask(
    val id: String = "",
    val kind: String = "",
    val status: String = "queued",
    @SerialName("status_text") val statusText: String = "Queued",
    val fault: String = "",
    @SerialName("after_reboot") val afterReboot: Boolean = false,
    @SerialName("can_confirm") val canConfirm: Boolean = false,
    @SerialName("can_cancel") val canCancel: Boolean = false,
) {
    val isQueued: Boolean get() = status == "queued" || status == "sending"
    val isFailed: Boolean get() = status == "failed"
    val isAccepted: Boolean get() = status == "accepted"
    val isConfirmed: Boolean get() = status == "confirmed"
    val isExpired: Boolean get() = status == "expired"
}

/** Why a check-in was held back: it came from a place the ONU is not bound to. It waits for Approve or Block. */
@Serializable
data class OnuQuarantine(
    val reason: String = "",
    val count: Int = 0,
    val ip: String = "",
    val mac: String = "",
    @SerialName("site_name") val siteName: String = "",
)

@Serializable
data class Onu(
    val id: String = "",
    val serial: String = "",
    val manufacturer: String = "",
    val model: String = "",
    @SerialName("data_model") val dataModel: String = "",
    @SerialName("site_id") val siteId: String = "",
    @SerialName("site_name") val siteName: String = "",
    @SerialName("mgmt_ip") val mgmtIp: String = "",
    val macs: List<String> = emptyList(),
    val online: Boolean = false,
    @SerialName("last_inform") val lastInform: String? = null,
    @SerialName("account_id") val accountId: String = "",
    @SerialName("account_code") val accountCode: String = "",
    val unit: String = "",
    val link: String = "",
    val wifi: List<OnuWifi> = emptyList(),
    val desired: OnuDesired = OnuDesired(),
    @SerialName("last_task") val lastTask: OnuTask? = null,
    @SerialName("in_inventory") val inInventory: Boolean = false,
    @SerialName("inventory_status") val inventoryStatus: String = "",
    val warnings: List<String> = emptyList(),
    @SerialName("use_macs") val useMacs: List<String> = emptyList(),
    /** Set while a check-in from an unexpected place waits for the owner (Approve or Block). */
    val quarantine: OnuQuarantine? = null,
    /** The box refused its own ACS login and uses the shared one: a weaker identity, not an error. */
    @SerialName("shared_login_only") val sharedLoginOnly: Boolean = false,
    /** The box left a house: a factory reset is queued and it never gets a house's Wi-Fi. */
    val returned: Boolean = false,
    val blocked: Boolean = false,
) {
    val isQuarantined: Boolean get() = quarantine != null
    val title: String get() = listOf(manufacturer, model).filter { it.isNotBlank() }.joinToString(" ").ifBlank { "ONU" }
    val isLinked: Boolean get() = accountId.isNotBlank()
}

/** GET /accounts/{id}/onu: [onu] is null when no ONU is linked to the house. */
@Serializable
data class AccountOnu(
    val onu: Onu? = null,
    val task: OnuTask? = null,
)

/** The answer to a Wi-Fi change, reboot or refresh. */
@Serializable
data class OnuTaskReply(val task: OnuTask? = null)

/** An ONU (kind ont) of the equipment register: a serial and WAN MAC pair. */
@Serializable
data class RegisterItem(
    val id: String = "",
    val kind: String = "",
    val serial: String = "",
    @SerialName("gpon_serial") val gponSerial: String = "",
    val mac: String = "",
    val model: String = "",
    val status: String = "",
    @SerialName("account_id") val accountId: String = "",
    @SerialName("site_id") val siteId: String = "",
    @SerialName("olt_id") val oltId: String = "",
    @SerialName("olt_name") val oltName: String = "",
    @SerialName("pon_port") val ponPort: Int = 0,
    val warnings: List<String> = emptyList(),
)
