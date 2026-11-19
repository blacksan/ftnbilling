package ke.co.ftw.ftnadmin.ui.more

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material3.Checkbox
import androidx.compose.material3.FilterChip
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.unit.dp
import ke.co.ftw.ftnadmin.core.network.ApiError
import ke.co.ftw.ftnadmin.core.network.ErrorCode
import ke.co.ftw.ftnadmin.data.routers.NetInterface
import ke.co.ftw.ftnadmin.data.routers.RouterInput
import ke.co.ftw.ftnadmin.data.routers.WanMode
import ke.co.ftw.ftnadmin.domain.Site
import ke.co.ftw.ftnadmin.ui.common.ChipLabel
import ke.co.ftw.ftnadmin.ui.common.ChipRow
import ke.co.ftw.ftnadmin.ui.common.SectionCard
import ke.co.ftw.ftnadmin.ui.common.userMessage

/** Server field names (`details.fields` keys) used to show a validation message under the right input. */
object RouterField {
    const val NAME = "name"
    const val WAN_INTERFACE = "wan_interface"
    const val WAN_MODE = "wan_mode"
    const val WAN_ADDRESS = "wan_address"
    const val WAN_GATEWAY = "wan_gateway"
    const val WAN_DNS = "wan_dns"
    const val PPPOE_USER = "pppoe_user"
    const val PPPOE_PASS = "pppoe_pass"
    const val LAN_PORTS = "lan_ports"
    const val LAN_SUBNET = "lan_subnet"
    const val OLT_INTERFACE = "olt_interface"
    const val VLAN_ID = "vlan_id"
    const val CUSTOMER_SUBNET = "customer_subnet"
    const val BENCH_PORT = "bench_port"
    const val WAN_CAP_MBPS = "wan_cap_mbps"
    const val SUPPORT_PHONE = "support_phone"
}

/** Labels and hints of the port fields (one place, so the form and its test agree). */
object RouterFormCopy {
    const val OLT_CONFIRM = "Customers on the OLT lose service until the OLT uses the new setting."
    const val OLT_LABEL = "OLT uplink port"
    const val OLT_HINT = "The port the OLT is plugged into: an SFP port (sfp-sfpplus1) or an Ethernet port (for example ether3)."
    const val CUSTOMER_PORT_LABEL = "Customer port (extra)"
    const val CUSTOMER_PORT_HINT = "A port where you can plug a customer device directly (for example ether5). The server sets it up."
}

/**
 * The router's real Ethernet, SFP and combo ports for the pickers, from its interface list; typed entry stays
 * possible. Bridges, VLANs and tunnels are left out.
 */
fun portChoices(interfaces: List<NetInterface>): List<String> =
    interfaces.filter { i ->
        i.type !in setOf("vlan", "bridge", "wg", "wireguard") &&
            (i.type == "ether" || listOf("ether", "sfp", "qsfp", "combo").any { i.name.startsWith(it, ignoreCase = true) })
    }.map { it.name }.distinct()

/** Text state of the router form (create and edit). */
data class RouterForm(
    val name: String = "",
    val wanInterface: String = "",
    val wanMode: String = WanMode.DHCP,
    val wanAddress: String = "",
    val wanGateway: String = "",
    val wanDns: String = "",
    val pppoeUser: String = "",
    /** Write-only: typed here, never filled from the server. */
    val pppoePass: String = "",
    /** The server already holds a password (`pppoe_pass_set`). */
    val pppoePassSet: Boolean = false,
    val pppoePassClear: Boolean = false,
    val lanPorts: String = "",
    val lanSubnet: String = "",
    val oltInterface: String = "",
    val vlan: String = "",
    val customerSubnet: String = "",
    val benchPort: String = "",
    val wanCap: String = "",
    val supportPhone: String = "",
) {
    /** True when the OLT port or the VLAN differs from [original]: the server then needs a confirm on a set-up router. */
    fun changesOlt(original: RouterForm): Boolean =
        oltInterface.trim() != original.oltInterface.trim() || (vlan.toIntOrNull() ?: 0) != (original.vlan.toIntOrNull() ?: 0)

    fun toInput(confirm: Boolean = false) = RouterInput(
        name = name,
        wanInterface = wanInterface,
        wanMode = wanMode,
        wanAddress = if (wanMode == WanMode.STATIC) wanAddress else "",
        wanGateway = if (wanMode == WanMode.STATIC) wanGateway else "",
        wanDns = if (wanMode == WanMode.STATIC) wanDns else "",
        pppoeUser = if (wanMode == WanMode.PPPOE) pppoeUser else "",
        pppoePass = if (wanMode == WanMode.PPPOE) pppoePass else "",
        pppoePassClear = pppoePassClear && pppoePass.isEmpty(),
        lanPorts = lanPorts,
        lanSubnet = lanSubnet,
        oltInterface = oltInterface,
        vlanId = vlan.toIntOrNull(),
        customerSubnet = customerSubnet,
        benchPort = benchPort,
        wanCapMbps = wanCap.toIntOrNull(),
        supportPhone = supportPhone,
        confirm = confirm,
    )

    companion object {
        fun from(s: Site) = RouterForm(
            name = s.label,
            wanInterface = s.wanInterface,
            wanMode = s.wanMode.ifBlank { WanMode.DHCP },
            wanAddress = s.wanAddress,
            wanGateway = s.wanGateway,
            wanDns = s.wanDns,
            pppoeUser = s.pppoeUser,
            pppoePassSet = s.pppoePassSet,
            lanPorts = s.lanPorts,
            lanSubnet = s.lanSubnet,
            oltInterface = s.oltInterface,
            vlan = if (s.vlanId > 0) s.vlanId.toString() else "",
            customerSubnet = s.customerSubnet,
            benchPort = s.benchPort,
            wanCap = if (s.wanCapMbps > 0) s.wanCapMbps.toString() else "",
            supportPhone = s.supportPhone,
        )
    }
}

/** Failure of a create/save call: per-field messages from a 400 `validation_failed`, else one message. */
data class FormFailure(val fields: Map<String, String> = emptyMap(), val message: String? = null)

fun Throwable.toFormFailure(): FormFailure {
    val e = this as? ApiError
    if (e != null && e.code == ErrorCode.VALIDATION_FAILED && e.fieldErrors.isNotEmpty()) {
        return FormFailure(e.fieldErrors, "Fix the highlighted fields.")
    }
    return FormFailure(message = userMessage())
}

/**
 * The router fields. [errors] maps server field names to messages and is shown under each input.
 * [onEdit] gets the server field name being edited (so its error clears) and the change to apply.
 */
@Composable
fun RouterFormFields(
    form: RouterForm,
    errors: Map<String, String>,
    onEdit: (String, (RouterForm) -> RouterForm) -> Unit,
    includeName: Boolean = true,
    /** The router's real ports, when it is joined; empty means typed entry only. */
    ports: List<String> = emptyList(),
) {
    @Composable
    fun field(
        key: String,
        label: String,
        value: String,
        keyboard: KeyboardType = KeyboardType.Text,
        hint: String? = null,
        update: RouterForm.(String) -> RouterForm,
    ) {
        val err = errors[key]
        OutlinedTextField(
            value = value,
            onValueChange = { v -> onEdit(key) { it.update(v) } },
            label = { Text(label) },
            singleLine = true,
            isError = err != null,
            supportingText = (err ?: hint)?.let { t -> { Text(t) } },
            keyboardOptions = KeyboardOptions(keyboardType = keyboard),
            modifier = Modifier.fillMaxWidth(),
        )
    }

    SectionCard("Router") {
        if (includeName) field(RouterField.NAME, "Router name", form.name) { copy(name = it) }
        field(RouterField.SUPPORT_PHONE, "Support phone (shown on the payment page)", form.supportPhone, KeyboardType.Phone) { copy(supportPhone = it) }
    }

    SectionCard("Internet (WAN)") {
        field(RouterField.WAN_INTERFACE, "WAN port", form.wanInterface, hint = "Default ether1") { copy(wanInterface = it) }
        Text("How the router gets internet", style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
        ChipRow(horizontalPadding = 0.dp) {
            WanMode.all.forEach { m ->
                FilterChip(
                    selected = form.wanMode == m,
                    onClick = { onEdit(RouterField.WAN_MODE) { it.copy(wanMode = m) } },
                    label = { ChipLabel(WanMode.label(m)) },
                )
            }
        }
        errors[RouterField.WAN_MODE]?.let { Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall) }
        when (form.wanMode) {
            WanMode.STATIC -> {
                field(RouterField.WAN_ADDRESS, "Address with prefix", form.wanAddress, hint = "For example 203.0.113.10/29") { copy(wanAddress = it) }
                field(RouterField.WAN_GATEWAY, "Gateway", form.wanGateway) { copy(wanGateway = it) }
                field(RouterField.WAN_DNS, "DNS servers (optional)", form.wanDns, hint = "Comma separated") { copy(wanDns = it) }
            }
            WanMode.PPPOE -> {
                field(RouterField.PPPOE_USER, "PPPoE user", form.pppoeUser) { copy(pppoeUser = it) }
                PppoePasswordField(form, errors[RouterField.PPPOE_PASS], onEdit)
            }
            else -> Text(
                "The router asks the provider for an address (DHCP).",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
    }

    SectionCard("Admin LAN") {
        field(RouterField.LAN_PORTS, "LAN ports", form.lanPorts, hint = "For WinBox or a laptop. Comma separated, default ether2") { copy(lanPorts = it) }
        field(RouterField.LAN_SUBNET, "LAN subnet", form.lanSubnet, hint = "Default 192.168.88.0/24") { copy(lanSubnet = it) }
    }

    SectionCard("Customers") {
        field(RouterField.OLT_INTERFACE, RouterFormCopy.OLT_LABEL, form.oltInterface, hint = RouterFormCopy.OLT_HINT) { copy(oltInterface = it) }
        PortPicker(ports, form.oltInterface) { p -> onEdit(RouterField.OLT_INTERFACE) { it.copy(oltInterface = p) } }
        field(RouterField.VLAN_ID, "VLAN (0 = untagged)", form.vlan, KeyboardType.Number) { copy(vlan = it.filter(Char::isDigit)) }
        field(RouterField.CUSTOMER_SUBNET, "Customer subnet", form.customerSubnet, hint = "Default 10.5.50.0/24") { copy(customerSubnet = it) }
        field(RouterField.BENCH_PORT, RouterFormCopy.CUSTOMER_PORT_LABEL, form.benchPort, hint = RouterFormCopy.CUSTOMER_PORT_HINT) { copy(benchPort = it) }
        PortPicker(ports, form.benchPort) { p -> onEdit(RouterField.BENCH_PORT) { it.copy(benchPort = if (it.benchPort == p) "" else p) } }
        field(RouterField.WAN_CAP_MBPS, "WAN capacity (Mbps)", form.wanCap, KeyboardType.Number) { copy(wanCap = it.filter(Char::isDigit)) }
    }
}

/** Chips of the router's own ports under a port field; nothing when the router has not answered. */
@Composable
private fun PortPicker(ports: List<String>, selected: String, onPick: (String) -> Unit) {
    if (ports.isEmpty()) return
    ChipRow(horizontalPadding = 0.dp) {
        ports.forEach { p ->
            FilterChip(selected = selected == p, onClick = { onPick(p) }, label = { ChipLabel(p) })
        }
    }
}

/** Write-only password: the stored one is never shown, only that one is set. */
@Composable
private fun PppoePasswordField(form: RouterForm, error: String?, onEdit: (String, (RouterForm) -> RouterForm) -> Unit) {
    Column(verticalArrangement = Arrangement.spacedBy(4.dp)) {
        if (form.pppoePassSet && !form.pppoePassClear) {
            Text("Password set", style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurface)
        }
        OutlinedTextField(
            value = form.pppoePass,
            onValueChange = { v -> onEdit(RouterField.PPPOE_PASS) { it.copy(pppoePass = v, pppoePassClear = false) } },
            label = { Text(if (form.pppoePassSet) "New PPPoE password (leave empty to keep)" else "PPPoE password") },
            singleLine = true,
            visualTransformation = PasswordVisualTransformation(),
            keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Password),
            isError = error != null,
            supportingText = error?.let { t -> { Text(t) } },
            modifier = Modifier.fillMaxWidth(),
        )
        if (form.pppoePassSet) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Checkbox(
                    checked = form.pppoePassClear,
                    onCheckedChange = { c -> onEdit(RouterField.PPPOE_PASS) { it.copy(pppoePassClear = c, pppoePass = if (c) "" else it.pppoePass) } },
                )
                Text("Remove the saved password", style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurface)
            }
        }
    }
}
