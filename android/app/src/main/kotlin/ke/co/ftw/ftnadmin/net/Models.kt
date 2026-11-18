package ke.co.ftw.ftnadmin.net

/** `GET /healthz` (public and in-tunnel). Missing fields default to "not ok". */
data class ServerHealth(
    val ok: Boolean,
    val routerOk: Boolean,
    val sitesTotal: Int,
    val sitesOnline: Int,
    val dbOk: Boolean,
    val mpesaEnabled: Boolean,
    val testMode: Boolean,
    val db: String,
)

data class FleetTotals(
    val sites: Int,
    val up: Int,
    val down: Int,
    val other: Int,
    val accountsActive: Int,
    val accountsTotal: Int,
    val revenueTodayKes: Long,
    val revenueMonthKes: Long,
    val openAlerts: Int,
    val unmatched: Int,
    val unregistered: Int,
    val failedMessages: Int,
    val pendingOrders: Int,
)

data class SiteSummary(
    val id: String,
    val name: String,
    /** One of up, down, paused, waiting, unknown. */
    val state: String,
    val since: String,
    val cause: String,
    val handshake: String,
    val wan: String,
    val active: Int,
    val total: Int,
    val todayKes: Long,
    val monthKes: Long,
    val alerts: Int,
    val muted: Boolean,
)

/** `GET /admin/api/summary`. */
data class FleetSummary(
    val version: Int,
    val now: String,
    val brand: String,
    val hasOps: Boolean,
    val totals: FleetTotals,
    val sites: List<SiteSummary>,
)

/** Result of a network fetch as the UI sees it. */
sealed interface Fetch<out T> {
    data object Loading : Fetch<Nothing>
    data class Ok<T>(val value: T) : Fetch<T>

    /** The server answered 404: it predates the endpoint. */
    data object ServerTooOld : Fetch<Nothing>

    /** No usable admin login (none saved, or the server said 401). */
    data object AuthNeeded : Fetch<Nothing>
    data class Failed(val message: String) : Fetch<Nothing>
}
