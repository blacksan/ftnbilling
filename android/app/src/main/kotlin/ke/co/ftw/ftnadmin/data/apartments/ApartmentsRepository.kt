package ke.co.ftw.ftnadmin.data.apartments

import ke.co.ftw.ftnadmin.core.network.ApiClient
import ke.co.ftw.ftnadmin.domain.Account
import ke.co.ftw.ftnadmin.domain.Apartment
import ke.co.ftw.ftnadmin.domain.ApartmentContact
import ke.co.ftw.ftnadmin.domain.ApartmentForm
import ke.co.ftw.ftnadmin.domain.Page
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put
import kotlinx.serialization.json.putJsonArray

/** Apartments (buildings), their routers and the people who look after them (docs/api/v6-apartments.md). */
class ApartmentsRepository(private val api: ApiClient) {
    suspend fun list(query: String? = null, routerId: String? = null): List<Apartment> =
        api.get<Page<Apartment>>("apartments", query = mapOf("q" to query?.ifBlank { null }, "router" to routerId, "limit" to "200")).items

    suspend fun get(id: String): Apartment = api.get("apartments", id)

    suspend fun create(form: ApartmentForm, routerIds: List<String>): Apartment =
        api.post("apartments", body = body(form, routerIds))

    /** Fields not sent stay as they are; an empty contact section clears it. */
    suspend fun update(id: String, form: ApartmentForm): Apartment = api.patch("apartments", id, body = body(form, null))

    /** 409 `apartment_in_use` while any account belongs to it. */
    suspend fun delete(id: String) = api.send("DELETE", "apartments", id)

    suspend fun link(id: String, routerId: String): Apartment =
        api.post("apartments", id, "routers", body = buildJsonObject { put("site_id", routerId) })

    /** 409 `router_serves_accounts` while open accounts of the apartment are on that router. */
    suspend fun unlink(id: String, routerId: String): Apartment = api.delete("apartments", id, "routers", routerId)

    /** Moves a customer: new apartment, router (from the apartment's routers; empty = its only one) and unit. */
    suspend fun moveAccount(accountId: String, apartmentId: String, routerId: String?, unit: String?): Account =
        api.post(
            "accounts", accountId, "move",
            body = buildJsonObject {
                put("apartment_id", apartmentId)
                if (!routerId.isNullOrBlank()) put("site_id", routerId)
                if (!unit.isNullOrBlank()) put("unit", unit.trim())
            },
        )

    private fun contact(c: ApartmentContact): JsonObject = buildJsonObject {
        put("name", c.name.trim())
        put("phone", c.phone.trim())
        put("phone2", c.phone2.trim())
        put("email", c.email.trim())
        put("notes", c.notes.trim())
    }

    private fun body(f: ApartmentForm, routerIds: List<String>?): JsonObject = buildJsonObject {
        put("name", f.name.trim())
        put("address", f.address.trim())
        put("notes", f.notes.trim())
        put("caretaker", contact(f.caretaker))
        put("landlord", contact(f.landlord))
        if (routerIds != null) putJsonArray("router_ids") { routerIds.forEach { add(kotlinx.serialization.json.JsonPrimitive(it)) } }
    }
}
