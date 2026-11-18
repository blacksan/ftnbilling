package ke.co.ftw.ftnadmin.data.payments

import ke.co.ftw.ftnadmin.core.network.ApiClient
import ke.co.ftw.ftnadmin.data.putIfNotBlank
import ke.co.ftw.ftnadmin.domain.Payment
import ke.co.ftw.ftnadmin.domain.PaymentPage
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put

class PaymentsRepository(private val api: ApiClient) {
    suspend fun list(
        siteId: String? = null,
        status: String? = null,
        query: String? = null,
        cursor: String? = null,
        limit: Int = 50,
    ): PaymentPage = api.get(
        "payments",
        query = mapOf("site" to siteId, "status" to status, "q" to query, "cursor" to cursor, "limit" to limit.toString()),
    )

    suspend fun record(
        accountId: String,
        amountKes: Long,
        transCode: String?,
        packageId: String?,
        note: String?,
        force: Boolean = false,
    ): Payment = api.post(
        "payments",
        body = buildJsonObject {
            put("account_id", accountId)
            put("amount_kes", amountKes)
            putIfNotBlank("trans_code", transCode)
            putIfNotBlank("package_id", packageId)
            putIfNotBlank("note", note)
            if (force) put("force", true)
        },
    )

    suspend fun assign(key: String, accountId: String, packageId: String, note: String?): Payment = api.post(
        "payments", key, "assign",
        body = buildJsonObject {
            put("account_id", accountId)
            put("package_id", packageId)
            putIfNotBlank("note", note)
        },
    )
}
