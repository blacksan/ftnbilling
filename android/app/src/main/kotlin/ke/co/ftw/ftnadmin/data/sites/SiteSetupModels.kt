package ke.co.ftw.ftnadmin.data.sites

import ke.co.ftw.ftnadmin.domain.Site
import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable

// Mirrors docs/api/v5-bootstrap.md (one bootstrap line, derived four-step setup view, no provisioning job).
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
    val physical: Boolean = false,
    /** "operator" or "owner": the lowest role allowed to run the fix. */
    val role: String = "operator",
)

@Serializable
data class CheckItem(
    val id: String = "",
    val group: String = "",
    val status: String = CheckStatus.UNKNOWN,
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

/** `GET /routers/{id}/setup` states (v5): derived by the server, nothing is run by a job. */
object SetupState {
    const val NOT_STARTED = "not_started"
    const val WAITING_ROUTER = "waiting_router"
    const val ONLINE = "online"
    const val READY = "ready"

    /** The app polls (every 5 s) until every step is green. */
    fun isReady(state: String) = state == READY

    fun label(state: String) = when (state) {
        "", NOT_STARTED -> "Not started"
        WAITING_ROUTER -> "Waiting for the router"
        ONLINE -> "Online, checking"
        READY -> "Ready"
        else -> state.replace('_', ' ').replaceFirstChar { it.uppercase() }
    }
}

/** The four derived steps of `GET /routers/{id}/setup`, in order. */
object SetupStepKey {
    const val SCRIPT_FETCHED = "script_fetched"
    const val TUNNEL_UP = "tunnel_up"
    const val SERVER_LOGIN = "server_login"
    const val CHECKS_GREEN = "checks_green"
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
data class NextAction(
    /** paste_line, press_button, fix, wait or none. */
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

/** `GET /routers/{id}/setup`: the four steps, the join state and what to do next. */
@Serializable
data class RouterSetup(
    @SerialName("router_id") val routerId: String = "",
    val state: String = "",
    val join: SetupJoin = SetupJoin(),
    @SerialName("next_action") val nextAction: NextAction = NextAction(),
    val steps: List<Step> = emptyList(),
    val checks: ChecksSummary = ChecksSummary(),
    val blockers: List<String> = emptyList(),
    @SerialName("last_error") val lastError: String = "",
) {
    /** True when all four steps are ok (the poll stops). */
    val allGreen: Boolean get() = state == SetupState.READY
}

/** `POST /routers/{id}/actions/reset`: the router record and a fresh one-time line. */
@Serializable
data class ResetResult(
    val router: Site = Site(),
    val join: JoinCommand = JoinCommand(),
)

@Serializable
data class SuspendTest(val steps: List<Step> = emptyList())
