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
        api.get("routers", siteId, "checks", query = mapOf("fresh" to if (fresh) "1" else null))

    suspend fun fix(siteId: String, checkId: String, confirm: Boolean): FixResult = api.post(
        "routers", siteId, "checks", checkId, "fix",
        body = buildJsonObject { if (confirm) put("confirm", true) },
    )

    /** 409 `bad_state` when the router looks like it is in another mode and [override] is false. */
    suspend fun setMode(siteId: String, mode: String, override: Boolean = false): Site = api.put(
        "routers", siteId, "mode",
        body = buildJsonObject {
            put("mode", mode)
            put("override", override)
        },
    )

    suspend fun joinCommand(siteId: String): JoinCommand = api.post("routers", siteId, "join-command")

    /** The automatic provisioning job for this router (poll while [SetupState.isLive]). */
    suspend fun setup(routerId: String): RouterSetup = api.get("routers", routerId, "setup")

    /** Retry or kick the automatic job. */
    suspend fun runSetup(routerId: String): RouterSetup = api.post("routers", routerId, "setup", "run")

    /** The single "Start migration" button for a router that is already serving customers. */
    suspend fun migrate(routerId: String, confirmName: String, keepUnknown: Boolean): RouterSetup = api.post(
        "routers", routerId, "setup", "migrate",
        body = buildJsonObject {
            put("confirm_name", confirmName)
            put("keep_unknown", keepUnknown)
        },
    )

    /** Emergency only: the router returns to hotspot mode within about 30 s. */
    suspend fun rollback(routerId: String): RouterSetup = api.post("routers", routerId, "setup", "rollback")

    /** Fresh read of every check (operator). */
    suspend fun recheck(routerId: String): Checks = api.post("routers", routerId, "actions", "recheck")

    /** Re-applies every account to the router (202, runs in the background). */
    suspend fun resync(routerId: String) = api.send("POST", "routers", routerId, "actions", "resync")

    /** Owner only. 409 `bad_state` while a job runs; the server also limits this to 1 per 10 minutes per router. */
    suspend fun reboot(routerId: String, confirmName: String) = api.send(
        "POST", "routers", routerId, "actions", "reboot",
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
