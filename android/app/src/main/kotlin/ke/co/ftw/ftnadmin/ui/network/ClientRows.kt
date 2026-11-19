package ke.co.ftw.ftnadmin.ui.network

import androidx.compose.animation.AnimatedVisibility
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.KeyboardArrowDown
import androidx.compose.material.icons.filled.KeyboardArrowUp
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import ke.co.ftw.ftnadmin.ui.theme.MonoFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import ke.co.ftw.ftnadmin.data.routers.Client
import ke.co.ftw.ftnadmin.data.routers.NetInterface
import ke.co.ftw.ftnadmin.ui.common.ChipRow
import ke.co.ftw.ftnadmin.ui.common.LabeledValue
import ke.co.ftw.ftnadmin.ui.common.StatusChip
import ke.co.ftw.ftnadmin.ui.common.StatusDot
import ke.co.ftw.ftnadmin.ui.common.Tone
import ke.co.ftw.ftnadmin.ui.common.formatAge
import ke.co.ftw.ftnadmin.ui.common.formatBps
import ke.co.ftw.ftnadmin.ui.common.formatBytes

private val Mono = MonoFamily

/** Collapsed: who, address, state. Expanded: lease, queue, usage, port, last seen and the actions. */
@OptIn(ExperimentalLayoutApi::class)
@Composable
internal fun ClientRow(
    c: Client,
    expanded: Boolean,
    busy: Boolean,
    canWrite: Boolean,
    onToggle: () -> Unit,
    onOpen: () -> Unit,
    onSuspend: () -> Unit,
    onResync: () -> Unit,
    onCreate: () -> Unit,
    onDismiss: () -> Unit,
) {
    Card(
        onClick = onToggle,
        modifier = Modifier.fillMaxWidth(),
        colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceContainerHigh),
    ) {
        Column(Modifier.padding(14.dp), verticalArrangement = Arrangement.spacedBy(6.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(10.dp)) {
                StatusDot(c.online, size = 12.dp)
                Column(Modifier.weight(1f)) {
                    Text(c.title, style = MaterialTheme.typography.titleMedium, fontWeight = FontWeight.Bold, maxLines = 1, overflow = TextOverflow.Ellipsis)
                    val who = listOf(c.tenant, c.hostName.takeIf { c.isFlat }).filter { !it.isNullOrBlank() }.joinToString(" - ")
                    if (who.isNotBlank()) Text(who, style = MaterialTheme.typography.bodyMedium, maxLines = 1, overflow = TextOverflow.Ellipsis)
                    Text(
                        listOf(c.ip, c.mac).filter { it.isNotBlank() }.joinToString("  "),
                        style = MaterialTheme.typography.bodySmall.copy(fontFamily = Mono),
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        maxLines = 1,
                        overflow = TextOverflow.Ellipsis,
                    )
                }
                Icon(if (expanded) Icons.Default.KeyboardArrowUp else Icons.Default.KeyboardArrowDown, contentDescription = if (expanded) "Collapse" else "Expand")
            }
            ChipRow(horizontalPadding = 0.dp) {
                if (!c.isFlat) StatusChip("Unregistered", Tone.Warn)
                StatusChip(if (c.online) "Online" else "Offline", if (c.online) Tone.Good else Tone.Neutral)
                if (c.vendor.isNotBlank()) StatusChip(c.vendor, Tone.Neutral)
                if (c.suspendedOnRouter) StatusChip("Walled", Tone.Bad)
                if (c.isFlat && !c.inSync) StatusChip("Out of sync", Tone.Warn)
            }
            AnimatedVisibility(expanded) {
                Column(verticalArrangement = Arrangement.spacedBy(4.dp)) {
                    LabeledValue("Lease", c.leaseStatus.replaceFirstChar { it.uppercase() })
                    if (c.queueLimit.isNotBlank()) LabeledValue("Speed limit", c.queueLimit)
                    LabeledValue("Now", "down ${formatBps(c.downBps)}, up ${formatBps(c.upBps)}")
                    if (c.isFlat) {
                        LabeledValue("Usage today", formatBytes(c.usageTodayBytes))
                        LabeledValue("Usage this month", formatBytes(c.usageMonthBytes))
                    }
                    if (c.port.isNotBlank()) LabeledValue("Port", c.port)
                    if (c.hostName.isNotBlank()) LabeledValue("Host name", c.hostName)
                    if (c.routerUser.isNotBlank()) LabeledValue("Router user", c.routerUser)
                    LabeledValue("Last seen", formatAge(c.lastSeen))
                    if (canWrite) {
                        FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                            if (c.isFlat && c.accountId.isNotBlank()) {
                                Button(onClick = onOpen) { Text("Open customer", maxLines = 1, softWrap = false) }
                                OutlinedButton(onClick = onSuspend, enabled = !busy) {
                                    Text(if (c.suspendedOnRouter) "Unsuspend" else "Suspend", maxLines = 1, softWrap = false)
                                }
                                OutlinedButton(onClick = onResync, enabled = !busy) { Text("Resync", maxLines = 1, softWrap = false) }
                                TextButton(onClick = onOpen) { Text("Change MAC", maxLines = 1, softWrap = false) }
                            } else if (!c.isFlat) {
                                Button(onClick = onCreate) { Text("Create customer", maxLines = 1, softWrap = false) }
                                OutlinedButton(onClick = onDismiss, enabled = !busy) { Text("Dismiss", maxLines = 1, softWrap = false) }
                            }
                        }
                    } else if (c.isFlat && c.accountId.isNotBlank()) {
                        TextButton(onClick = onOpen) { Text("Open customer", maxLines = 1, softWrap = false) }
                    }
                }
            }
        }
    }
}

@Composable
internal fun InterfaceRow(i: NetInterface, expanded: Boolean, onToggle: () -> Unit) {
    Card(
        onClick = onToggle,
        modifier = Modifier.fillMaxWidth(),
        colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceContainerHigh),
    ) {
        Column(Modifier.padding(14.dp), verticalArrangement = Arrangement.spacedBy(6.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(10.dp)) {
                StatusDot(if (i.disabled) null else i.running, size = 12.dp)
                Column(Modifier.weight(1f)) {
                    Text(i.name, style = MaterialTheme.typography.titleMedium, fontWeight = FontWeight.Bold, maxLines = 1, overflow = TextOverflow.Ellipsis)
                    Text(
                        if (i.rxBps != null || i.txBps != null) "down ${formatBps(i.rxBps ?: 0)}, up ${formatBps(i.txBps ?: 0)}" else "Rates appear after the next refresh",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        maxLines = 1,
                        overflow = TextOverflow.Ellipsis,
                    )
                }
                Icon(if (expanded) Icons.Default.KeyboardArrowUp else Icons.Default.KeyboardArrowDown, contentDescription = if (expanded) "Collapse" else "Expand")
            }
            ChipRow(horizontalPadding = 0.dp) {
                StatusChip(roleLabel(i.role), roleTone(i.role))
                StatusChip(if (i.disabled) "Disabled" else if (i.running) "Running" else "Down", if (i.disabled) Tone.Neutral else if (i.running) Tone.Good else Tone.Bad)
                if (i.type.isNotBlank()) StatusChip(i.type, Tone.Neutral)
            }
            AnimatedVisibility(expanded) {
                Column(verticalArrangement = Arrangement.spacedBy(4.dp)) {
                    if (i.comment.isNotBlank()) LabeledValue("Comment", i.comment)
                    if (i.mac.isNotBlank()) LabeledValue("MAC", i.mac)
                    if (i.mtu > 0) LabeledValue("MTU", i.mtu.toString())
                    if (i.addresses.isNotEmpty()) LabeledValue("Addresses", i.addresses.joinToString("\n"))
                    LabeledValue("Received", formatBytes(i.rxBytes))
                    LabeledValue("Sent", formatBytes(i.txBytes))
                    i.lastLinkUp?.let { LabeledValue("Last link up", formatAge(it)) }
                }
            }
        }
    }
}

internal fun roleLabel(role: String) = when (role) {
    "wan" -> "WAN"
    "olt" -> "OLT"
    "customer" -> "Customers"
    "tunnel" -> "Tunnel"
    "bench" -> "Bench"
    else -> "Other"
}

private fun roleTone(role: String) = when (role) {
    "wan" -> Tone.Good
    "olt", "customer" -> Tone.Neutral
    "bench" -> Tone.Warn
    else -> Tone.Neutral
}
