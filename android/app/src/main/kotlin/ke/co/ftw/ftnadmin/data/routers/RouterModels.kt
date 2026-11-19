package ke.co.ftw.ftnadmin.data.routers

import ke.co.ftw.ftnadmin.data.sites.JoinCommand
import ke.co.ftw.ftnadmin.domain.Site
import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable

// API v4 shapes (release plan section 3). Every field has a default so a server that adds, drops
// or nulls a field never crashes the app. Secrets never appear in these types.

@Serializable
data class RouterSystem(
    val board: String = "",
    val model: String = "",
    val serial: String = "",
    val version: String = "",
    val arch: String = "",
    @SerialName("cpu_count") val cpuCount: Int = 0,
    @SerialName("cpu_load_pct") val cpuLoadPct: Double = 0.0,
    @SerialName("mem_used_bytes") val memUsedBytes: Long = 0,
    @SerialName("mem_total_bytes") val memTotalBytes: Long = 0,
    @SerialName("disk_free_bytes") val diskFreeBytes: Long = 0,
    @SerialName("uptime_s") val uptimeS: Long = 0,
    @SerialName("temp_c") val tempC: Double? = null,
) {
    val memPct: Double get() = if (memTotalBytes > 0) memUsedBytes * 100.0 / memTotalBytes else 0.0
}

@Serializable
data class RouterWan(
    @SerialName("interface") val iface: String = "",
    val running: Boolean = true,
    @SerialName("down_bps") val downBps: Long = 0,
    @SerialName("up_bps") val upBps: Long = 0,
    @SerialName("cap_mbps") val capMbps: Int = 0,
    @SerialName("util_pct") val utilPct: Double = 0.0,
)

@Serializable
data class RouterTunnel(
    @SerialName("handshake_age_s") val handshakeAgeS: Long? = null,
    @SerialName("rtt_ms") val rttMs: Double? = null,
    @SerialName("loss_pct") val lossPct: Double? = null,
)

@Serializable
data class Throughput(
    @SerialName("step_s") val stepS: Int = 300,
    @SerialName("down_bps") val downBps: List<Long> = emptyList(),
    @SerialName("up_bps") val upBps: List<Long> = emptyList(),
)

@Serializable
data class FlatsSummary(
    val total: Int = 0,
    val online: Int = 0,
    val active: Int = 0,
    val expired: Int = 0,
    val suspended: Int = 0,
    @SerialName("expiring_7d") val expiring7d: Int = 0,
)

@Serializable
data class EquipmentCounts(val olt: Int = 0, val ont: Int = 0)

@Serializable
data class LiveRevenue(
    @SerialName("today_kes") val todayKes: Long = 0,
    @SerialName("month_kes") val monthKes: Long = 0,
)

@Serializable
data class LiveProvision(
    @SerialName("in_sync") val inSync: Boolean = true,
    val drift: Int = 0,
    @SerialName("pending_ops") val pendingOps: Int = 0,
    @SerialName("last_error") val lastError: String = "",
)

/** `GET /routers/{id}/live`. [reachable] false means the numbers are the last known ones. */
@Serializable
data class RouterLive(
    @SerialName("router_id") val routerId: String = "",
    @SerialName("generated_at") val generatedAt: String = "",
    val reachable: Boolean = true,
    val system: RouterSystem = RouterSystem(),
    val wan: RouterWan = RouterWan(),
    val tunnel: RouterTunnel = RouterTunnel(),
    val throughput: Throughput = Throughput(),
    val flats: FlatsSummary = FlatsSummary(),
    @SerialName("unregistered_online") val unregisteredOnline: Int = 0,
    @SerialName("onts_bound") val ontsBound: Int = 0,
    val equipment: EquipmentCounts = EquipmentCounts(),
    val revenue: LiveRevenue = LiveRevenue(),
    @SerialName("open_alerts") val openAlerts: Int = 0,
    val provision: LiveProvision = LiveProvision(),
    @SerialName("setup_state") val setupState: String = "",
)

@Serializable
data class NetInterface(
    val name: String = "",
    val type: String = "",
    val running: Boolean = true,
    val disabled: Boolean = false,
    val comment: String = "",
    val mac: String = "",
    val mtu: Int = 0,
    /** wan, olt, customer, tunnel, bench or other. */
    val role: String = "other",
    @SerialName("rx_bps") val rxBps: Long? = null,
    @SerialName("tx_bps") val txBps: Long? = null,
    @SerialName("rx_bytes") val rxBytes: Long = 0,
    @SerialName("tx_bytes") val txBytes: Long = 0,
    val addresses: List<String> = emptyList(),
    @SerialName("last_link_up") val lastLinkUp: String? = null,
)

@Serializable
data class InterfacesResponse(
    val items: List<NetInterface> = emptyList(),
    @SerialName("generated_at") val generatedAt: String = "",
)

object ClientKind {
    const val FLAT = "flat"
    const val UNREGISTERED = "unregistered"
}

/** One device on the router: a flat's ONT or an unregistered MAC. */
@Serializable
data class Client(
    val kind: String = ClientKind.FLAT,
    @SerialName("account_id") val accountId: String = "",
    val code: String = "",
    val flat: String = "",
    val tenant: String = "",
    val state: String = "",
    val mac: String = "",
    val vendor: String = "",
    @SerialName("host_name") val hostName: String = "",
    val ip: String = "",
    val online: Boolean = false,
    @SerialName("lease_status") val leaseStatus: String = "none",
    @SerialName("last_seen") val lastSeen: String? = null,
    val port: String = "",
    @SerialName("queue_limit") val queueLimit: String = "",
    @SerialName("down_bps") val downBps: Long = 0,
    @SerialName("up_bps") val upBps: Long = 0,
    @SerialName("usage_today_bytes") val usageTodayBytes: Long = 0,
    @SerialName("usage_month_bytes") val usageMonthBytes: Long = 0,
    @SerialName("suspended_on_router") val suspendedOnRouter: Boolean = false,
    @SerialName("in_sync") val inSync: Boolean = true,
    @SerialName("router_user") val routerUser: String = "",
) {
    val isFlat: Boolean get() = kind != ClientKind.UNREGISTERED

    /** Stable list key: flats by account, unregistered devices by MAC. */
    val key: String get() = if (isFlat && accountId.isNotBlank()) "a-$accountId" else "m-$mac"

    val title: String
        get() = when {
            isFlat && flat.isNotBlank() -> "Flat $flat"
            hostName.isNotBlank() -> hostName
            vendor.isNotBlank() -> vendor
            else -> mac.ifBlank { "Unknown device" }
        }

    /** The text the search box matches against (the server also matches `q`). */
    fun matches(q: String): Boolean {
        val n = q.trim().lowercase()
        if (n.isEmpty()) return true
        return listOf(flat, tenant, code, ip, mac, vendor, hostName).any { it.lowercase().contains(n) }
    }
}

@Serializable
data class ClientCounts(val flats: Int = 0, val online: Int = 0, val unregistered: Int = 0)

@Serializable
data class ClientsResponse(
    val items: List<Client> = emptyList(),
    val counts: ClientCounts = ClientCounts(),
)

@Serializable
data class OverviewRevenue(
    @SerialName("today_kes") val todayKes: Long = 0,
    @SerialName("yesterday_kes") val yesterdayKes: Long = 0,
    @SerialName("month_kes") val monthKes: Long = 0,
    @SerialName("spark_7d") val spark7d: List<Long> = emptyList(),
)

@Serializable
data class OverviewRouters(val total: Int = 0, val online: Int = 0)

@Serializable
data class BusyRouter(
    @SerialName("router_id") val routerId: String = "",
    val name: String = "",
    @SerialName("down_bps") val downBps: Long = 0,
    @SerialName("util_pct") val utilPct: Double = 0.0,
    val online: Int = 0,
)

@Serializable
data class NextItem(val kind: String = "", val text: String = "", val count: Int = 0, val href: String = "")

@Serializable
data class OverviewHealth(
    @SerialName("at_risk") val atRisk: Int = 0,
    val churned: Int = 0,
    @SerialName("expiring_7d") val expiring7d: Int = 0,
)

/** `GET /overview`: fleet totals for the "All routers" home. */
@Serializable
data class Overview(
    val revenue: OverviewRevenue = OverviewRevenue(),
    val active: Int = 0,
    @SerialName("online_now") val onlineNow: Int = 0,
    val routers: OverviewRouters = OverviewRouters(),
    @SerialName("busiest_routers") val busiestRouters: List<BusyRouter> = emptyList(),
    val next: List<NextItem> = emptyList(),
    val health: OverviewHealth = OverviewHealth(),
)

object WanMode {
    const val DHCP = "dhcp"
    const val STATIC = "static"
    const val PPPOE = "pppoe"
    val all = listOf(DHCP, STATIC, PPPOE)

    fun label(mode: String) = when (mode) {
        DHCP -> "DHCP"
        STATIC -> "Static IP"
        PPPOE -> "PPPoE"
        else -> mode
    }
}

/**
 * The router record body of `POST /routers` and `PATCH /routers/{id}` (docs/api/v5-bootstrap.md).
 * [pppoePass] is write-only; [pppoePassClear] removes the stored one.
 */
data class RouterInput(
    val name: String,
    val wanInterface: String = "",
    val wanMode: String = WanMode.DHCP,
    val wanAddress: String = "",
    val wanGateway: String = "",
    val wanDns: String = "",
    val pppoeUser: String = "",
    val pppoePass: String = "",
    val pppoePassClear: Boolean = false,
    val lanPorts: String = "",
    val lanSubnet: String = "",
    val oltInterface: String = "",
    val vlanId: Int? = null,
    val customerSubnet: String = "",
    val benchPort: String = "",
    val wanCapMbps: Int? = null,
    val supportPhone: String = "",
)

/** `POST /routers`: 201, never replayed. The join command is a one-time secret; keep it in memory only. */
@Serializable
data class CreatedRouter(
    val router: Site = Site(),
    val join: JoinCommand = JoinCommand(),
)
