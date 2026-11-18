package ke.co.ftw.ftnadmin.data.sites

import ke.co.ftw.ftnadmin.core.network.ApiClient
import ke.co.ftw.ftnadmin.domain.ItemList
import ke.co.ftw.ftnadmin.domain.Ont
import ke.co.ftw.ftnadmin.domain.Qoe
import ke.co.ftw.ftnadmin.domain.Site

class SitesRepository(private val api: ApiClient) {
    suspend fun sites(): List<Site> = api.get<ItemList<Site>>("sites").items

    suspend fun site(id: String): Site = api.get("sites", id)

    suspend fun onts(id: String): List<Ont> = api.get<ItemList<Ont>>("sites", id, "onts").items

    suspend fun qoe(id: String, period: String): Qoe = api.get("sites", id, "qoe", query = mapOf("period" to period))
}
