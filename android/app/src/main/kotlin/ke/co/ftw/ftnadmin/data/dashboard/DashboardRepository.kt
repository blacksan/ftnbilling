package ke.co.ftw.ftnadmin.data.dashboard

import ke.co.ftw.ftnadmin.core.network.ApiClient
import ke.co.ftw.ftnadmin.domain.Dashboard

class DashboardRepository(private val api: ApiClient) {
    suspend fun dashboard(siteId: String? = null): Dashboard = api.get("dashboard", query = mapOf("router" to siteId))
}
