package ke.co.ftw.ftnadmin.data.networkmap

import ke.co.ftw.ftnadmin.core.network.ApiClient

/** The Network map: apartments, their markers' numbers and the equipment installed in them. Read only. */
class NetworkMapRepository(private val api: ApiClient) {
    suspend fun map(): NetworkMap = api.get("network", "map")
}
