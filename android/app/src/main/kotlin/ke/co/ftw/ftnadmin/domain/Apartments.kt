package ke.co.ftw.ftnadmin.domain

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable

// Apartments (API v6, docs/api/v6-apartments.md). An apartment is a building; customers have a unit in it.
// Routers and apartments are many to many.

@Serializable
data class AptRef(val id: String = "", val name: String = "")

/** Caretaker or landlord of an apartment. Phones are +254 numbers; [whatsappUrl] is a wa.me link. */
@Serializable
data class ApartmentContact(
    val name: String = "",
    val phone: String = "",
    val phone2: String = "",
    val email: String = "",
    val notes: String = "",
    @SerialName("whatsapp_url") val whatsappUrl: String = "",
) {
    val isEmpty: Boolean get() = name.isBlank() && phone.isBlank() && phone2.isBlank() && email.isBlank() && notes.isBlank()

    /** tel: link for [phone], or null when there is none. */
    val telUri: String? get() = phone.takeIf { it.isNotBlank() }?.let { "tel:$it" }
}

@Serializable
data class ApartmentRouter(
    @SerialName("site_id") val siteId: String = "",
    @SerialName("site_name") val siteName: String = "",
    @SerialName("router_id") val routerId: String = "",
    @SerialName("router_name") val routerName: String = "",
    /** Open accounts of this apartment on that router. */
    val accounts: Int = 0,
) {
    val key: String get() = routerId.ifBlank { siteId }
    val label: String get() = routerName.ifBlank { siteName }
}

/** List rows and the detail answer share this shape; detail adds [accounts]. */
@Serializable
data class Apartment(
    val id: String = "",
    val name: String = "",
    val address: String = "",
    val notes: String = "",
    val caretaker: ApartmentContact = ApartmentContact(),
    val landlord: ApartmentContact = ApartmentContact(),
    val routers: List<ApartmentRouter> = emptyList(),
    @SerialName("accounts_total") val accountsTotal: Int = 0,
    @SerialName("accounts_active") val accountsActive: Int = 0,
    @SerialName("online_now") val onlineNow: Int = 0,
    val accounts: List<Account> = emptyList(),
    /** GPS point of the building; both null when unknown (0,0 is a real point). */
    val lat: Double? = null,
    val lng: Double? = null,
    @SerialName("map_url") val mapUrl: String? = null,
) {
    val ref: AptRef get() = AptRef(id, name)

    /** The building's point, or null when it has none. */
    val gps: GpsPoint? get() = if (lat != null && lng != null) GpsPoint(lat, lng) else null
}

@Serializable
data class MoveChoice(val id: String = "", val name: String = "")

/**
 * New devices: a MAC that belongs to an open account of another apartment. It is never provisioned on the
 * router that sees it; the operator can move the customer here.
 */
@Serializable
data class BelongsTo(
    @SerialName("account_id") val accountId: String = "",
    val code: String = "",
    val customer: String = "",
    val apartment: AptRef = AptRef(),
    val unit: String = "",
    @SerialName("site_id") val siteId: String = "",
    @SerialName("site_name") val siteName: String = "",
    /** The seeing router also serves the customer's apartment: moving changes only the router. */
    @SerialName("same_apartment") val sameApartment: Boolean = false,
    @SerialName("move_to_choices") val moveToChoices: List<MoveChoice> = emptyList(),
) {
    /** "Belongs to Wanjiru (Alpha, unit A-1)". */
    val text: String
        get() {
            val who = customer.ifBlank { "a customer" }
            val where = listOf(apartment.name, "unit ${unit.ifBlank { "?" }}").filter { it.isNotBlank() }.joinToString(", ")
            return "Belongs to $who ($where)"
        }
}

/** Input for create and edit of an apartment. */
data class ApartmentForm(
    val name: String = "",
    val address: String = "",
    val notes: String = "",
    val caretaker: ApartmentContact = ApartmentContact(),
    val landlord: ApartmentContact = ApartmentContact(),
    /** GPS point; null clears it on the server. */
    val gps: GpsPoint? = null,
)
