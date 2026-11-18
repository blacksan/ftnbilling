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

object CutoverState {
    const val IDLE = "idle"
    const val RUNNING = "running"
    const val AWAITING_CONFIRM = "awaiting_confirm"
    const val CONFIRMED = "confirmed"
    const val ROLLING_BACK = "rolling_back"
    const val ROLLED_BACK = "rolled_back"
    const val FAILED = "failed"

    /** States in which the server is still working or waiting on a person: the app polls. */
    fun isLive(state: String) = state == RUNNING || state == AWAITING_CONFIRM || state == ROLLING_BACK
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
data class CutoverEvidence(
    @SerialName("tunnel_up") val tunnelUp: Boolean = false,
    @SerialName("flats_bound") val flatsBound: Int = 0,
    @SerialName("flats_entitled") val flatsEntitled: Int = 0,
    @SerialName("internet_ok") val internetOk: Boolean = false,
)

@Serializable
data class Cutover(
    val state: String = CutoverState.IDLE,
    val method: String = "server",
    @SerialName("started_at") val startedAt: String? = null,
    @SerialName("deadline_at") val deadlineAt: String? = null,
    @SerialName("seconds_left") val secondsLeft: Int = 0,
    val steps: List<Step> = emptyList(),
    val evidence: CutoverEvidence = CutoverEvidence(),
    @SerialName("can_confirm") val canConfirm: Boolean = false,
    val blockers: List<String> = emptyList(),
    @SerialName("last_error") val lastError: String = "",
)

@Serializable
data class SuspendTest(val steps: List<Step> = emptyList())
