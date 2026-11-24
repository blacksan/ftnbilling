package ke.co.ftw.ftnadmin.data.routers

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable

/** Roles of a port (`GET /routers/{id}/ports`). The server is the source of truth; WAN is locked. */
object PortRole {
    const val WAN = "wan"
    const val LAN = "lan"
    const val OLT = "olt"
    const val OLT_MGMT = "oltmgmt"
    const val CUSTOMER = "customer"
    const val UNUSED = "unused"

    fun label(role: String): String = when (role) {
        WAN -> "Internet uplink (WAN)"
        LAN -> "Admin LAN"
        OLT -> "OLT uplink"
        OLT_MGMT -> "OLT management"
        CUSTOMER -> "Customer port"
        UNUSED -> "Unused"
        else -> role
    }
}

@Serializable
data class PortApply(
    val port: String = "",
    /** ok, applying or error. */
    val state: String = "ok",
    val message: String = "",
)

/** One physical port of a router. */
@Serializable
data class PortRow(
    val name: String = "",
    /** ethernet, sfp, combo or wlan. */
    val kind: String = "ethernet",
    val role: String = PortRole.UNUSED,
    @SerialName("role_label") val roleLabel: String = "",
    val locked: Boolean = false,
    val running: Boolean = false,
    val link: String = "",
    val speed: String = "",
    val duplex: String = "",
    val disabled: Boolean = false,
    val mac: String = "",
    @SerialName("rx_bps") val rxBps: Long? = null,
    @SerialName("tx_bps") val txBps: Long? = null,
    val comment: String = "",
    val apply: PortApply? = null,
    val flats: Int? = null,
    @SerialName("can_change") val canChange: Boolean = false,
    /** False for the OLT uplink: it is never switched off from here (give the OLT role to another port first). */
    @SerialName("can_disable") val canDisable: Boolean = true,
    @SerialName("needs_owner") val needsOwner: Boolean = false,
    @SerialName("role_options") val roleOptions: List<String> = emptyList(),
    @SerialName("disable_warning") val disableWarning: String = "",
    @SerialName("role_warning") val roleWarning: String = "",
    @SerialName("confirm_disable") val confirmDisable: Boolean = false,
    /** Ports are off until the owner enables them; false = the server keeps this one switched off. */
    @SerialName("owner_enabled") val ownerEnabled: Boolean = true,
    /** Fixed line for the port's role, e.g. "Admin — router management and the billing site only". */
    val note: String = "",
    /** A hint about the port's state, e.g. "Up with no role — disable it if you don't use it". */
    val warning: String = "",
)

@Serializable
data class PortsResponse(
    val items: List<PortRow> = emptyList(),
    val reachable: Boolean = true,
    @SerialName("generated_at") val generatedAt: String = "",
    /** What the server did with the customer ports and the admin LAN (same object as on the router). */
    @SerialName("customer_port") val customerPort: ke.co.ftw.ftnadmin.domain.CustomerPortStatus? = null,
)

@Serializable
data class PortChangeResponse(val applied: List<String> = emptyList())

/** What the person has to accept before a port change goes through. */
data class PortConfirm(val title: String, val warning: String, val needsName: Boolean)

/** The typed-name rule of the server, mirrored so the dialog is asked first. */
object PortRules {
    fun disable(p: PortRow): PortConfirm =
        if (p.confirmDisable) PortConfirm("Disable ${p.name}?", p.disableWarning, needsName = true)
        else PortConfirm("Disable ${p.name}?", p.disableWarning.ifBlank { "${p.name} stops carrying traffic until you enable it." }, needsName = false)

    fun role(p: PortRow, role: String): PortConfirm {
        val label = PortRole.label(role)
        val needsName = p.role == PortRole.LAN || role == PortRole.OLT
        val warning = when {
            p.role == PortRole.LAN -> p.roleWarning
            role == PortRole.OLT -> "All customers on the OLT go offline while the OLT uplink moves to ${p.name}."
            else -> "The server sets it up on the router by itself."
        }
        return PortConfirm("Make ${p.name} $label?", warning, needsName)
    }
}
