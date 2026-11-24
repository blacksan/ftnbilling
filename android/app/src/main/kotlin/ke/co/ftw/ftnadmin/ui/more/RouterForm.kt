package ke.co.ftw.ftnadmin.ui.more

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material3.TextButton
import androidx.compose.material3.FilterChip
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.res.stringResource
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
import ke.co.ftw.ftnadmin.R
import ke.co.ftw.ftnadmin.ui.common.CheckRow
import ke.co.ftw.ftnadmin.ui.common.LabeledValue
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
    const val CUSTOMER_PORTS = "customer_ports"
    const val CONFIRM_NAME = "confirm_name"
    const val WAN_CAP_MBPS = "wan_cap_mbps"
    const val SUPPORT_PHONE = "support_phone"
}

/** Labels and hints of the port fields (one place, so the form and its test agree). */
object RouterFormCopy {
    const val OLT_CONFIRM = "Customers on the OLT lose service until the OLT uses the new setting."
    const val PAUSED_NOTE = "Automatic fixes are paused; this change is still applied now."
    /** The OLT port a blank field means on the server. */
    const val DEFAULT_OLT = "sfp-sfpplus1"
    const val OLT_LABEL = "OLT uplink port"
    const val OLT_HINT = "The port the OLT is plugged into: an SFP port (sfp-sfpplus1) or an Ethernet port (for example ether3)."
    const val CUSTOMER_PORT_LABEL = "Customer ports (extra)"
    const val CUSTOMER_PORT_HINT = "Ports where you can plug a customer device directly (for example ether4,ether5). The server sets them up."
    const val LAN_CONFIRM = "You may lose your WinBox session on this port. The server keeps the old admin LAN port until the new one has a link."
    const val DEFAULT_LAN = "ether2"
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

/** A comma separated port list as a list: trimmed, no blanks, no repeats. */
fun portList(text: String): List<String> = text.split(',').map { it.trim() }.filter { it.isNotEmpty() }.distinct()

/** [list] with [port] added, or removed when it is there already. */
fun togglePort(list: String, port: String): String {
    val l = portList(list)
    return (if (port in l) l - port else l + port).joinToString(",")
}

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
    /** Comma separated customer ports (the server keeps a list; `bench_port` is only its first entry). */
    val customerPorts: String = "",
    val wanCap: String = "",
    val supportPhone: String = "",
) {
    /** True when the OLT port or the VLAN differs from [original]: the server then needs a confirm on a set-up router. */
    fun changesOlt(original: RouterForm): Boolean =
        oltInterface.trim().ifEmpty { RouterFormCopy.DEFAULT_OLT } != original.oltInterface.trim().ifEmpty { RouterFormCopy.DEFAULT_OLT } || (vlan.toIntOrNull() ?: 0) != (original.vlan.toIntOrNull() ?: 0)

    /** True when the admin LAN ports differ from [original]: the server then needs the typed router name. */
    fun changesLan(original: RouterForm): Boolean = portList(lanPorts.ifBlank { RouterFormCopy.DEFAULT_LAN }) != portList(original.lanPorts.ifBlank { RouterFormCopy.DEFAULT_LAN })

    fun toInput(confirm: Boolean = false, confirmName: String = "") = RouterInput(
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
        customerPorts = portList(customerPorts),
        wanCapMbps = wanCap.toIntOrNull(),
        supportPhone = supportPhone,
        confirm = confirm,
        confirmName = confirmName,
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
            customerPorts = s.customerPorts.ifEmpty { listOfNotNull(s.benchPort.ifBlank { null }) }.joinToString(","),
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
@OptIn(ExperimentalLayoutApi::class)
@Composable
fun RouterFormFields(
    form: RouterForm,
    errors: Map<String, String>,
    onEdit: (String, (RouterForm) -> RouterForm) -> Unit,
    includeName: Boolean = true,
    /** The router's real ports, when it is joined; empty means typed entry only. */
    ports: List<String> = emptyList(),
    /** Editing an existing router: the OLT and customer ports are shown, not edited. The Ports screen is the one place to change them. */
    portsReadOnly: Boolean = false,
    onOpenPorts: (() -> Unit)? = null,
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
        field(RouterField.SUPPORT_PHONE, "Support phone", form.supportPhone, KeyboardType.Phone, hint = "Shown on the payment page") { copy(supportPhone = it) }
    }

    SectionCard("Internet (WAN)") {
        field(RouterField.WAN_INTERFACE, "WAN port", form.wanInterface, hint = "Default ether1") { copy(wanInterface = it) }
        Text("How the router gets internet", style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
        FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            WanMode.all.forEach { m ->
                FilterChip(
                    selected = form.wanMode == m,
                    onClick = { onEdit(RouterField.WAN_MODE) { it.copy(wanMode = m) } },
                    label = { Text(WanMode.label(m)) },
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
        if (portsReadOnly) {
            // One owner for port roles: the Ports screen (it confirms before it changes anything). Here they are only shown.
            LabeledValue(RouterFormCopy.OLT_LABEL, form.oltInterface.ifBlank { RouterFormCopy.DEFAULT_OLT }, mono = true)
            LabeledValue(RouterFormCopy.CUSTOMER_PORT_LABEL, form.customerPorts.ifBlank { "-" }, mono = true)
            Text(stringResource(R.string.ports_change_in_ports), style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
            if (onOpenPorts != null) TextButton(onClick = onOpenPorts) { Text(stringResource(R.string.open_ports)) }
        } else if (ports.isEmpty()) {
            // The router has not reported its ports (a new router): type them.
            field(RouterField.OLT_INTERFACE, RouterFormCopy.OLT_LABEL, form.oltInterface, hint = RouterFormCopy.OLT_HINT) { copy(oltInterface = it) }
        } else {
            Text(RouterFormCopy.OLT_LABEL, style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
            PortPicker(ports, listOf(form.oltInterface)) { p -> onEdit(RouterField.OLT_INTERFACE) { it.copy(oltInterface = p) } }
            Text(RouterFormCopy.OLT_HINT, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
        field(RouterField.VLAN_ID, "VLAN (0 = untagged)", form.vlan, KeyboardType.Number) { copy(vlan = it.filter(Char::isDigit)) }
        field(RouterField.CUSTOMER_SUBNET, "Customer subnet", form.customerSubnet, hint = "Default 10.5.50.0/24") { copy(customerSubnet = it) }
        if (!portsReadOnly) {
            if (ports.isEmpty()) {
                field(RouterField.CUSTOMER_PORTS, RouterFormCopy.CUSTOMER_PORT_LABEL, form.customerPorts, hint = RouterFormCopy.CUSTOMER_PORT_HINT) { copy(customerPorts = it) }
            } else {
                Text(RouterFormCopy.CUSTOMER_PORT_LABEL, style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
                PortPicker(ports.filter { it != form.wanInterface }, portList(form.customerPorts)) { p -> onEdit(RouterField.CUSTOMER_PORTS) { it.copy(customerPorts = togglePort(it.customerPorts, p)) } }
                Text(RouterFormCopy.CUSTOMER_PORT_HINT, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            }
        }
        field(RouterField.WAN_CAP_MBPS, "WAN capacity (Mbps)", form.wanCap, KeyboardType.Number) { copy(wanCap = it.filter(Char::isDigit)) }
    }
}

/** Chips of the router's own ports; nothing when the router has not answered. They wrap, so every port is in view. */
@OptIn(ExperimentalLayoutApi::class)
@Composable
private fun PortPicker(ports: List<String>, selected: List<String>, onPick: (String) -> Unit) {
    if (ports.isEmpty()) return
    FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
        ports.forEach { p ->
            FilterChip(selected = p in selected, onClick = { onPick(p) }, label = { Text(p) })
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
            label = { Text(if (form.pppoePassSet) "New PPPoE password" else "PPPoE password") },
            singleLine = true,
            visualTransformation = PasswordVisualTransformation(),
            keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Password),
            isError = error != null,
            supportingText = (error ?: if (form.pppoePassSet) "Leave empty to keep the saved password." else null)?.let { t -> { Text(t) } },
            modifier = Modifier.fillMaxWidth(),
        )
        if (form.pppoePassSet) {
            CheckRow(
                checked = form.pppoePassClear,
                onChange = { c -> onEdit(RouterField.PPPOE_PASS) { it.copy(pppoePassClear = c, pppoePass = if (c) "" else it.pppoePass) } },
                label = "Remove the saved password",
            )
        }
    }
}
