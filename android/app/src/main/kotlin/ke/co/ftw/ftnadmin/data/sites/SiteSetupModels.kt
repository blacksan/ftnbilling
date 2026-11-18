package ke.co.ftw.ftnadmin.data.sites

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable

// Mirrors docs/setup-automation-plan.md section 6 (router onboarding automation).
// Every field has a default so a server that adds, drops or nulls a field never crashes the app.

object CheckStatus {
    const val OK = "ok"
    const val WARN = "warn"
    const val FAIL = "fail"
    const val UNKNOWN = "unknown"
    const val INFO = "info"
}

object FixKind {
    const val AUTO = "auto"
    const val CONFIRM = "confirm"
    const val MANUAL = "manual"
    const val NONE = "none"
}

object SiteMode {
    const val HOTSPOT = "hotspot"
    const val IPOE_OBSERVE = "ipoe_observe"
    const val IPOE = "ipoe"
    val all = listOf(HOTSPOT, IPOE_OBSERVE, IPOE)

    fun label(mode: String) = when (mode) {
        HOTSPOT -> "Hotspot"
        IPOE_OBSERVE -> "IPoE observe"
        IPOE -> "IPoE"
        else -> mode
    }
}

@Serializable
data class RouterInfo(
    val reachable: Boolean = false,
    val version: String = "",
    val board: String = "",
)

@Serializable
data class ChecksSummary(
    val ok: Int = 0,
    val warn: Int = 0,
    val fail: Int = 0,
    val unknown: Int = 0,
    @SerialName("cutover_ready") val cutoverReady: Boolean = false,
)

@Serializable
data class JoinReport(
    val at: String = "",
    val ok: Boolean = false,
    val lines: List<String> = emptyList(),
)

@Serializable
data class CheckFix(
    val kind: String = FixKind.NONE,
    val label: String = "",
    @SerialName("confirm_text") val confirmText: String = "",
    val command: String = "",
    val physical: Boolean = false,
    /** "operator" or "owner": the lowest role allowed to run the fix. */
    val role: String = "operator",
)

@Serializable
data class CheckItem(
    val id: String = "",
    val group: String = "",
    val status: String = CheckStatus.UNKNOWN,
    @SerialName("blocks_cutover") val blocksCutover: Boolean = false,
    val title: String = "",
    val detail: String = "",
    val evidence: List<String> = emptyList(),
    val fix: CheckFix? = null,
)

@Serializable
data class Checks(
    @SerialName("site_id") val siteId: String = "",
    @SerialName("generated_at") val generatedAt: String = "",
    val router: RouterInfo = RouterInfo(),
    val summary: ChecksSummary = ChecksSummary(),
    @SerialName("join_report") val joinReport: JoinReport? = null,
    val items: List<CheckItem> = emptyList(),
)

@Serializable
data class FixResult(
    val item: CheckItem = CheckItem(),
    val applied: List<String> = emptyList(),
)

@Serializable
data class JoinCommand(
    val command: String = "",
    @SerialName("command_nocheck") val commandNocheck: String = "",
    @SerialName("expires_at") val expiresAt: String = "",
)

/** Job states of the router provisioning job (release plan section 2.2). Unknown states are treated as "working". */
object SetupState {
    const val WAITING_ROUTER = "waiting_router"
    const val CHECKING = "checking"
    const val FIXING = "fixing"
    const val NEEDS_ACTION = "needs_action"
    const val STAGING = "staging"
    const val BUILDING = "building"
    const val READY = "ready"
    const val ARMING = "arming"
    const val SWITCHING = "switching"
    const val VERIFYING = "verifying"
    const val LIVE = "live"
    const val ROLLING_BACK = "rolling_back"
    const val ROLLED_BACK = "rolled_back"
    const val FAILED = "failed"

    private val settled = setOf("", "idle", NEEDS_ACTION, READY, LIVE, ROLLED_BACK, FAILED, "confirmed")

    /** States in which the server is still working on its own: the app polls. */
    fun isLive(state: String) = state !in settled

    /** States after the old setup has been touched, where an emergency rollback makes sense. */
    fun canRollBack(state: String) = state == ARMING || state == SWITCHING || state == VERIFYING

    fun label(state: String) = when (state) {
        WAITING_ROUTER -> "Waiting for the router"
        CHECKING -> "Checking"
        FIXING -> "Fixing"
        NEEDS_ACTION -> "Needs your action"
        STAGING -> "Preparing"
        BUILDING -> "Building the network"
        READY -> "Ready to migrate"
        ARMING -> "Arming the safety net"
        SWITCHING -> "Switching over"
        VERIFYING -> "Verifying"
        LIVE -> "Live"
        ROLLING_BACK -> "Rolling back"
        ROLLED_BACK -> "Rolled back"
        FAILED -> "Failed"
        "", "idle" -> "Not started"
        else -> state.replace('_', ' ').replaceFirstChar { it.uppercase() }
    }
}

@Serializable
data class Step(
    val n: Int = 0,
    val key: String = "",
    val title: String = "",
    val status: String = "pending",
    val detail: String = "",
    val at: String? = null,
)

@Serializable
data class SetupEvidence(
    @SerialName("tunnel_up") val tunnelUp: Boolean = false,
    @SerialName("internet_ok") val internetOk: Boolean = false,
    @SerialName("flats_required") val flatsRequired: Int = 0,
    @SerialName("flats_bound") val flatsBound: Int = 0,
)

@Serializable
data class NextAction(
    /** paste_join, press_button, fix, migrate, retry or none. */
    val kind: String = "none",
    val text: String = "",
    @SerialName("check_id") val checkId: String = "",
)

@Serializable
data class SetupJoin(
    val state: String = "none",
    @SerialName("expires_at") val expiresAt: String? = null,
    val report: JoinReport? = null,
)

/** `GET /routers/{id}/setup`: the whole automatic job in one view. No manual confirm exists (amendment A1). */
@Serializable
data class RouterSetup(
    @SerialName("router_id") val routerId: String = "",
    val state: String = "",
    /** fresh, migrate or empty. */
    val kind: String = "",
    @SerialName("serving_customers") val servingCustomers: Boolean = false,
    @SerialName("auto_start") val autoStart: Boolean = false,
    val join: SetupJoin = SetupJoin(),
    @SerialName("next_action") val nextAction: NextAction = NextAction(),
    val steps: List<Step> = emptyList(),
    val checks: ChecksSummary = ChecksSummary(),
    val blockers: List<String> = emptyList(),
    @SerialName("live_devices") val liveDevices: Int = 0,
    val evidence: SetupEvidence = SetupEvidence(),
    @SerialName("deadline_at") val deadlineAt: String? = null,
    @SerialName("seconds_left") val secondsLeft: Int = 0,
    @SerialName("last_error") val lastError: String = "",
)

@Serializable
data class SuspendTest(val steps: List<Step> = emptyList())
