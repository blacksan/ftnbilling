package ke.co.ftw.ftnadmin.net

import org.json.JSONException
import org.json.JSONObject

/** Tolerant parsers: unknown fields are ignored, missing ones fall back to neutral defaults. */
object JsonParsers {
    fun parseHealth(body: String): ServerHealth? = try {
        val o = JSONObject(body)
        ServerHealth(
            ok = o.optBoolean("ok", false),
            routerOk = o.optBoolean("router_ok", false),
            sitesTotal = o.optInt("sites_total", 0),
            sitesOnline = o.optInt("sites_online", 0),
            dbOk = o.optBoolean("db_ok", false),
            mpesaEnabled = o.optBoolean("mpesa_enabled", false),
            testMode = o.optBoolean("test_mode", false),
            db = o.optString("db", ""),
        )
    } catch (_: JSONException) {
        null
    }

    fun parseSummary(body: String): FleetSummary? = try {
        val o = JSONObject(body)
        val t = o.optJSONObject("totals") ?: JSONObject()
        val sitesJson = o.optJSONArray("sites")
        val sites = buildList {
            if (sitesJson != null) {
                for (i in 0 until sitesJson.length()) {
                    val s = sitesJson.optJSONObject(i) ?: continue
                    add(
                        SiteSummary(
                            id = s.optString("id", ""),
                            name = s.optString("name", ""),
                            state = s.optString("state", "unknown"),
                            since = s.optString("since", ""),
                            cause = s.optString("cause", ""),
                            handshake = s.optString("handshake", ""),
                            wan = s.optString("wan", ""),
                            active = s.optInt("active", 0),
                            total = s.optInt("total", 0),
                            todayKes = s.optLong("today_kes", 0),
                            monthKes = s.optLong("month_kes", 0),
                            alerts = s.optInt("alerts", 0),
                            muted = s.optBoolean("muted", false),
                        ),
                    )
                }
            }
        }
        FleetSummary(
            version = o.optInt("v", 0),
            now = o.optString("now", ""),
            brand = o.optString("brand", ""),
            hasOps = o.optBoolean("has_ops", false),
            totals = FleetTotals(
                sites = t.optInt("sites", sites.size),
                up = t.optInt("up", 0),
                down = t.optInt("down", 0),
                other = t.optInt("other", 0),
                accountsActive = t.optInt("accounts_active", 0),
                accountsTotal = t.optInt("accounts_total", 0),
                revenueTodayKes = t.optLong("revenue_today_kes", 0),
                revenueMonthKes = t.optLong("revenue_month_kes", 0),
                openAlerts = t.optInt("open_alerts", 0),
                unmatched = t.optInt("unmatched", 0),
                unregistered = t.optInt("unregistered", 0),
                failedMessages = t.optInt("failed_messages", 0),
                pendingOrders = t.optInt("pending_orders", 0),
            ),
            sites = sites,
        )
    } catch (_: JSONException) {
        null
    }
}
