package ke.co.ftw.ftnadmin.data.sites

import ke.co.ftw.ftnadmin.core.network.ApiClient
import ke.co.ftw.ftnadmin.domain.Account
import ke.co.ftw.ftnadmin.domain.Site
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put

/**
 * Router setup, cutover and bench calls (setup automation plan, section 6).
 *
 * Nothing here caches: every call goes to the server. [joinCommand] returns a secret-bearing
 * one-time command that callers must keep only in memory and never log.
 */
class SiteSetupRepository(private val api: ApiClient) {
    /** [fresh] bypasses the server's 15 s cache (`?fresh=1`). */
    suspend fun checks(siteId: String, fresh: Boolean = false): Checks =
        api.get("sites", siteId, "checks", query = mapOf("fresh" to if (fresh) "1" else null))

    suspend fun fix(siteId: String, checkId: String, confirm: Boolean): FixResult = api.post(
        "sites", siteId, "checks", checkId, "fix",
        body = buildJsonObject { if (confirm) put("confirm", true) },
    )

    /** 409 `bad_state` when the router looks like it is in another mode and [override] is false. */
    suspend fun setMode(siteId: String, mode: String, override: Boolean = false): Site = api.put(
        "sites", siteId, "mode",
        body = buildJsonObject {
            put("mode", mode)
            put("override", override)
        },
    )

    suspend fun joinCommand(siteId: String): JoinCommand = api.post("sites", siteId, "join-command")

    suspend fun cutover(siteId: String): Cutover = api.get("sites", siteId, "cutover")

    suspend fun startCutover(siteId: String, confirmName: String, keepUnknown: Boolean): Cutover = api.post(
        "sites", siteId, "cutover", "start",
        body = buildJsonObject {
            put("confirm_name", confirmName)
            put("keep_unknown", keepUnknown)
        },
    )

    suspend fun confirmCutover(siteId: String, force: Boolean, confirmName: String?): Cutover = api.post(
        "sites", siteId, "cutover", "confirm",
        body = buildJsonObject {
            put("force", force)
            if (force && confirmName != null) put("confirm_name", confirmName)
        },
    )

    suspend fun rollbackCutover(siteId: String): Cutover = api.post("sites", siteId, "cutover", "rollback")

    suspend fun testFlat(siteId: String, mac: String, packageId: String): Account = api.post(
        "sites", siteId, "bench", "test-flat",
        body = buildJsonObject {
            put("mac", mac)
            put("package_id", packageId)
        },
    )

    suspend fun suspendTest(siteId: String, accountId: String): SuspendTest = api.post(
        "sites", siteId, "bench", "suspend-test",
        body = buildJsonObject { put("account_id", accountId) },
    )
}
