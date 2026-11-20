package ke.co.ftw.ftnadmin.domain

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable

// Models mirror docs/api-v1.md section B. Every field has a default so a server that adds, drops
// or nulls a field never crashes the app.

@Serializable
data class Page<T>(
    val items: List<T> = emptyList(),
    @SerialName("next_cursor") val nextCursor: String? = null,
)

@Serializable
data class ItemList<T>(val items: List<T> = emptyList())

@Serializable
data class PackageRef(
    val id: String = "",
    val name: String = "",
    @SerialName("speed_mbps") val speedMbps: Int = 0,
    @SerialName("price_kes") val priceKes: Long = 0,
)

@Serializable
data class RouterState(
    @SerialName("in_sync") val inSync: Boolean = true,
    @SerialName("suspended_on_router") val suspendedOnRouter: Boolean = false,
    @SerialName("lease_status") val leaseStatus: String = "none",
    @SerialName("last_seen") val lastSeen: String? = null,
    @SerialName("pending_task") val pendingTask: Boolean = false,
    @SerialName("last_error") val lastError: String = "",
)

@Serializable
data class Customer(
    val id: String = "",
    val name: String = "",
    val phone: String = "",
    val email: String = "",
)

@Serializable
data class AccountEvent(
    val time: String = "",
    val msg: String = "",
    val actor: String = "",
)

@Serializable
data class Usage(@SerialName("month_bytes") val monthBytes: Long = 0)

/** List rows and the detail response share this shape; detail adds [customer], [payments], [events] and [usage]. */
@Serializable
data class Account(
    val id: String = "",
    val code: String = "",
    @SerialName("site_id") val siteId: String = "",
    @SerialName("site_name") val siteName: String = "",
    @SerialName("router_id") val routerId: String = "",
    @SerialName("router_name") val routerName: String = "",
    val flat: String = "",
    /** The apartment (building) and the unit in it; null for an account that has no apartment yet. */
    val apartment: AptRef? = null,
    val unit: String = "",
    /** Why the server is not provisioning the account, in words; empty when fine. */
    val problem: String = "",
    val floor: String = "",
    val mac: String = "",
    @SerialName("mac_conflict") val macConflict: Boolean = false,
    val ip: String = "",
    val tenant: String = "",
    val phone: String = "",
    val whatsapp: String = "",
    val state: String = "new",
    val suspended: Boolean = false,
    @SerialName("package") val pkg: PackageRef? = null,
    @SerialName("expires_at") val expiresAt: String? = null,
    val router: RouterState? = null,
    @SerialName("customer_id") val customerId: String = "",
    @SerialName("created_at") val createdAt: String = "",
    val customer: Customer? = null,
    val payments: List<Payment> = emptyList(),
    val events: List<AccountEvent> = emptyList(),
    val usage: Usage? = null,
) {
    /** v4 servers send router_id; older ones only site_id. */
    val routerKey: String get() = routerId.ifBlank { siteId }
    val routerLabel: String get() = routerName.ifBlank { siteName }
    val unitLabel: String get() = unit.ifBlank { flat }
    val title: String get() = listOf(unitLabel, tenant).filter { it.isNotBlank() }.joinToString(" - ").ifBlank { code }
}

@Serializable
data class ServicePackage(
    val id: String = "",
    val name: String = "",
    @SerialName("speed_mbps") val speedMbps: Int = 0,
    @SerialName("duration_min") val durationMin: Int = 0,
    @SerialName("price_kes") val priceKes: Long = 0,
    val disabled: Boolean = false,
)

@Serializable
data class Payment(
    val key: String = "",
    @SerialName("site_id") val siteId: String = "",
    @SerialName("router_id") val routerId: String = "",
    @SerialName("router_name") val routerName: String = "",
    val method: String = "",
    val status: String = "",
    @SerialName("amount_kes") val amountKes: Long = 0,
    val phone: String = "",
    val name: String = "",
    @SerialName("bill_ref") val billRef: String = "",
    @SerialName("account_id") val accountId: String = "",
    @SerialName("account_code") val accountCode: String = "",
    val flat: String = "",
    @SerialName("package_id") val packageId: String = "",
    val note: String = "",
    @SerialName("paid_at") val paidAt: String? = null,
    @SerialName("created_at") val createdAt: String = "",
) {
    val routerKey: String get() = routerId.ifBlank { siteId }
    val routerLabel: String get() = routerName

    /** Server statuses: matched, unmatched, refunded, ignored. */
    val isUnmatched: Boolean get() = status == "unmatched"
}

@Serializable
data class PaymentPage(
    val items: List<Payment> = emptyList(),
    @SerialName("next_cursor") val nextCursor: String? = null,
    val totals: PaymentTotals = PaymentTotals(),
)

@Serializable
data class PaymentTotals(
    val count: Int = 0,
    @SerialName("revenue_kes") val revenueKes: Long = 0,
)

@Serializable
data class AccountCounts(
    val active: Int = 0,
    val expired: Int = 0,
    val suspended: Int = 0,
    val new: Int = 0,
    @SerialName("mac_conflict") val macConflict: Int = 0,
)

@Serializable
data class Revenue(
    @SerialName("today_kes") val todayKes: Long = 0,
    @SerialName("month_kes") val monthKes: Long = 0,
    @SerialName("last_month_kes") val lastMonthKes: Long = 0,
)

@Serializable
data class Dashboard(
    @SerialName("sites_total") val sitesTotal: Int = 0,
    @SerialName("sites_online") val sitesOnline: Int = 0,
    val accounts: AccountCounts = AccountCounts(),
    val revenue: Revenue = Revenue(),
    @SerialName("unmatched_payments") val unmatchedPayments: Int = 0,
    @SerialName("unregistered_devices") val unregisteredDevices: Int = 0,
    @SerialName("open_alerts") val openAlerts: Int = 0,
    @SerialName("pending_router_tasks") val pendingRouterTasks: Int = 0,
    @SerialName("generated_at") val generatedAt: String = "",
)

@Serializable
data class SiteTunnel(@SerialName("handshake_age_s") val handshakeAgeS: Long? = null)

@Serializable
data class SiteAccounts(
    val total: Int = 0,
    val active: Int = 0,
    val expired: Int = 0,
    val suspended: Int = 0,
)

@Serializable
data class Provision(
    @SerialName("last_run_at") val lastRunAt: String? = null,
    @SerialName("in_sync") val inSync: Boolean = true,
    val drift: Int = 0,
    @SerialName("pending_ops") val pendingOps: Int = 0,
    @SerialName("last_error") val lastError: String = "",
    @SerialName("foreign_conflicts") val foreignConflicts: List<String> = emptyList(),
)

@Serializable
data class Site(
    val id: String = "",
    val name: String = "",
    @SerialName("router_id") val routerId: String = "",
    @SerialName("router_name") val routerName: String = "",
    @SerialName("setup_state") val setupState: String = "",
    // Router record (API v5). The WAN password is never returned: only whether one is set.
    @SerialName("wan_interface") val wanInterface: String = "",
    @SerialName("wan_mode") val wanMode: String = "dhcp",
    @SerialName("wan_address") val wanAddress: String = "",
    @SerialName("wan_gateway") val wanGateway: String = "",
    @SerialName("wan_dns") val wanDns: String = "",
    @SerialName("pppoe_user") val pppoeUser: String = "",
    @SerialName("pppoe_pass_set") val pppoePassSet: Boolean = false,
    @SerialName("lan_ports") val lanPorts: String = "",
    @SerialName("lan_subnet") val lanSubnet: String = "",
    /** The router's address inside the WireGuard tunnel; empty until it has fetched its script. */
    @SerialName("tunnel_address") val tunnelAddress: String = "",
    /** True only when the router reported being built with remote WinBox for the current management subnet. */
    @SerialName("mgmt_winbox") val mgmtWinbox: Boolean = false,
    /**
     * The admin's own page for opening this router's WebFig through the server (owners only; the page asks for
     * the password and authenticator code again). Empty when remote WebFig is off on the server or the router has no tunnel.
     */
    @SerialName("webfig_url") val webfigUrl: String = "",
    @SerialName("olt_interface") val oltInterface: String = "",
    @SerialName("vlan_id") val vlanId: Int = 0,
    @SerialName("customer_subnet") val customerSubnet: String = "",
    @SerialName("bench_port") val benchPort: String = "",
    /** Every customer port the server sets up (`bench_port` is only the first one, kept for older clients). */
    @SerialName("customer_ports") val customerPorts: List<String> = emptyList(),
    /** Ports the server keeps switched off (customer and unused ports only). */
    @SerialName("disabled_ports") val disabledPorts: List<String> = emptyList(),
    /** What the server did with the extra customer port; null from an older server. */
    @SerialName("customer_port") val customerPort: CustomerPortStatus? = null,
    /** Whether the server's automatic fixes of this router are paused; null from an older server. */
    @SerialName("auto_fixes") val autoFixes: AutoFixesPause? = null,
    @SerialName("wan_cap_mbps") val wanCapMbps: Int = 0,
    @SerialName("support_phone") val supportPhone: String = "",
    val online: Boolean = false,
    val cause: String = "",
    val tunnel: SiteTunnel = SiteTunnel(),
    val accounts: SiteAccounts = SiteAccounts(),
    @SerialName("open_alerts") val openAlerts: Int = 0,
    val provision: Provision? = null,
    @SerialName("uptime_30d_pct") val uptime30dPct: Double? = null,
) {
    /** The router's id: `router_id` when the server sends it, else the older `id`. */
    val key: String get() = routerId.ifBlank { id }
    val label: String get() = routerName.ifBlank { name }
}

@Serializable
data class WanStats(
    @SerialName("down_bps") val downBps: Long = 0,
    @SerialName("up_bps") val upBps: Long = 0,
    @SerialName("cap_mbps") val capMbps: Int = 0,
    @SerialName("peak_util_pct") val peakUtilPct: Double = 0.0,
)

@Serializable
data class QoeSeries(
    val t: List<String> = emptyList(),
    @SerialName("down_bps") val downBps: List<Long> = emptyList(),
    @SerialName("up_bps") val upBps: List<Long> = emptyList(),
    @SerialName("rtt_ms") val rttMs: List<Double> = emptyList(),
)

@Serializable
data class QoeFlag(val kind: String = "", val text: String = "")

@Serializable
data class Qoe(
    @SerialName("latency_ms") val latencyMs: Double = 0.0,
    @SerialName("loss_pct") val lossPct: Double = 0.0,
    val wan: WanStats = WanStats(),
    val series: QoeSeries = QoeSeries(),
    val flags: List<QoeFlag> = emptyList(),
)

@Serializable
data class Ont(
    @SerialName("account_id") val accountId: String = "",
    val code: String = "",
    val flat: String = "",
    val mac: String = "",
    val ip: String = "",
    @SerialName("lease_status") val leaseStatus: String = "none",
    @SerialName("last_seen") val lastSeen: String? = null,
    @SerialName("host_name") val hostName: String = "",
    @SerialName("down_bps") val downBps: Long = 0,
    @SerialName("up_bps") val upBps: Long = 0,
    @SerialName("suspended_on_router") val suspendedOnRouter: Boolean = false,
)

@Serializable
data class Alert(
    val key: String = "",
    val rule: String = "",
    val label: String = "",
    @SerialName("site_id") val siteId: String = "",
    @SerialName("site_name") val siteName: String = "",
    @SerialName("router_id") val routerId: String = "",
    @SerialName("router_name") val routerName: String = "",
    val state: String = "",
    val since: String = "",
    val detail: String = "",
) {
    val routerKey: String get() = routerId.ifBlank { siteId }
    val routerLabel: String get() = routerName.ifBlank { siteName }
}

@Serializable
data class AlertMute(
    @SerialName("site_id") val siteId: String = "",
    @SerialName("router_id") val routerId: String = "",
    val until: String = "",
) {
    val routerKey: String get() = routerId.ifBlank { siteId }
}

@Serializable
data class Alerts(
    val active: List<Alert> = emptyList(),
    val history: List<Alert> = emptyList(),
    val mutes: List<AlertMute> = emptyList(),
)

@Serializable
data class DeviceSession(
    val id: String = "",
    val name: String = "",
    val model: String = "",
    @SerialName("app_version") val appVersion: String = "",
    @SerialName("created_at") val createdAt: String = "",
    @SerialName("last_seen_at") val lastSeenAt: String? = null,
    @SerialName("last_ip") val lastIp: String = "",
    val current: Boolean = false,
    @SerialName("revoked_at") val revokedAt: String? = null,
)

@Serializable
data class SuggestedAccount(
    val id: String = "",
    val code: String = "",
    val flat: String = "",
)

/** A MAC that is connected to a router's network but not configured on any account. */
@Serializable
data class NewDevice(
    @SerialName("site_id") val siteId: String = "",
    @SerialName("site_name") val siteName: String = "",
    @SerialName("router_id") val routerId: String = "",
    @SerialName("router_name") val routerName: String = "",
    val mac: String = "",
    val vendor: String = "",
    @SerialName("host_name") val hostName: String = "",
    val ip: String = "",
    val source: String = "",
    val online: Boolean = false,
    @SerialName("first_seen") val firstSeen: String? = null,
    @SerialName("last_seen") val lastSeen: String? = null,
    val hits: Int = 0,
    @SerialName("suggested_account") val suggestedAccount: SuggestedAccount? = null,
    /** Set when the MAC belongs to a customer of another apartment: not provisioned here. */
    @SerialName("belongs_to") val belongsTo: BelongsTo? = null,
) {
    val routerKey: String get() = routerId.ifBlank { siteId }
    val routerLabel: String get() = routerName.ifBlank { siteName }
}

/** The pause of a router's automatic fixes as the router JSON carries it. */
@Serializable
data class AutoFixesPause(val paused: Boolean = false, val until: String? = null)

/** The server's apply status of the router's extra customer port (`customer_port` of a router). */
@Serializable
data class CustomerPortStatus(
    val port: String = "",
    /** off, ok, applying, error or rebuild. */
    val state: String = "off",
    /** Ready to show: "ether5: customer port ✓", "ether5: applying…" or the error in words. */
    val text: String = "",
    val message: String = "",
    @SerialName("olt_message") val oltMessage: String = "",
)

/** The line shown on the router page for the extra customer port; null when there is nothing to say. */
fun customerPortLine(st: CustomerPortStatus?): String? = st?.text?.takeIf { it.isNotBlank() }
