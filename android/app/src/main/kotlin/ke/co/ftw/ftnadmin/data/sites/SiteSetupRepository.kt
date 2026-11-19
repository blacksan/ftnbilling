package ke.co.ftw.ftnadmin.data.sites

import ke.co.ftw.ftnadmin.core.network.ApiClient
import ke.co.ftw.ftnadmin.domain.Account
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put

/**
 * Router setup view, checks, actions and bench calls (API v5).
 *
 * Nothing here caches: every call goes to the server. [joinCommand] returns a secret-bearing
 * one-time command that callers must keep only in memory and never log.
 */
class SiteSetupRepository(private val api: ApiClient) {
    /** [fresh] bypasses the server's 15 s cache (`?fresh=1`). */
    suspend fun checks(siteId: String, fresh: Boolean = false): Checks =
        api.get("routers", siteId, "checks", query = mapOf("fresh" to if (fresh) "1" else null))

    suspend fun fix(siteId: String, checkId: String, confirm: Boolean): FixResult = api.post(
        "routers", siteId, "checks", checkId, "fix",
        body = buildJsonObject { if (confirm) put("confirm", true) },
    )

    suspend fun joinCommand(siteId: String): JoinCommand = api.post("routers", siteId, "join-command")

    /** The four derived setup steps; the server only reads its own state and cached checks. */
    suspend fun setup(routerId: String): RouterSetup = api.get("routers", routerId, "setup")

    /** Fresh read of every check (operator). */
    suspend fun recheck(routerId: String): Checks = api.post("routers", routerId, "actions", "recheck")

    /** Re-applies every account to the router (202, runs in the background). */
    suspend fun resync(routerId: String) = api.send("POST", "routers", routerId, "actions", "resync")

    /** Owner only. 409 `bad_state` while a job runs; the server also limits this to 1 per 10 minutes per router. */
    suspend fun reboot(routerId: String, confirmName: String) = api.send(
        "POST", "routers", routerId, "actions", "reboot",
        body = buildJsonObject { put("confirm_name", confirmName) },
    )

    /** Owner only. Wrong [confirmName] is 400 `validation_failed`. Answers a fresh one-time line. */
    suspend fun reset(routerId: String, confirmName: String): ResetResult = api.post(
        "routers", routerId, "actions", "reset",
        body = buildJsonObject { put("confirm_name", confirmName) },
    )

    suspend fun testFlat(siteId: String, mac: String, packageId: String): Account = api.post(
        "routers", siteId, "bench", "test-flat",
        body = buildJsonObject {
            put("mac", mac)
            put("package_id", packageId)
        },
    )

    suspend fun suspendTest(siteId: String, accountId: String): SuspendTest = api.post(
        "routers", siteId, "bench", "suspend-test",
        body = buildJsonObject { put("account_id", accountId) },
    )
}
