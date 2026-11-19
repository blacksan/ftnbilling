package ke.co.ftw.ftnadmin.data.alerts

import ke.co.ftw.ftnadmin.core.network.ApiClient
import ke.co.ftw.ftnadmin.domain.Alerts
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put

class AlertsRepository(private val api: ApiClient) {
    suspend fun alerts(): Alerts = api.get("alerts")

    /** [minutes] = 0 unmutes. */
    suspend fun mute(siteId: String, minutes: Int) = api.send(
        "POST", "alerts", "mute",
        body = buildJsonObject {
            put("site_id", siteId)
            put("minutes", minutes)
        },
    )
}
