package ke.co.ftw.ftnadmin.data.sites

import ke.co.ftw.ftnadmin.core.network.ApiClient
import ke.co.ftw.ftnadmin.domain.ItemList
import ke.co.ftw.ftnadmin.domain.Ont
import ke.co.ftw.ftnadmin.domain.Qoe
import ke.co.ftw.ftnadmin.domain.Site

/** Routers (the `/routers` aliases of the old `/sites` paths; fields fall back to `site_*`). */
class SitesRepository(private val api: ApiClient) {
    suspend fun sites(): List<Site> = api.get<ItemList<Site>>("routers").items

    suspend fun site(id: String): Site = api.get("routers", id)

    suspend fun onts(id: String): List<Ont> = api.get<ItemList<Ont>>("routers", id, "onts").items

    suspend fun qoe(id: String, period: String): Qoe = api.get("routers", id, "qoe", query = mapOf("period" to period))
}
