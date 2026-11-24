package ke.co.ftw.ftnadmin.ui.common

/**
 * Plain-words labels for raw server values, in one place so the screens never show "mpesa" or "owner" as is.
 * They are pure functions: the unit tests call them without a Context.
 */

/** "mpesa" -> "M-Pesa". Unknown methods get their first letter capitalised ("card" -> "Card"). */
fun paymentMethodLabel(raw: String): String = when (raw.trim().lowercase()) {
    "" -> ""
    "mpesa", "m-pesa", "m_pesa" -> "M-Pesa"
    "cash" -> "Cash"
    "bank", "bank_transfer" -> "Bank transfer"
    "manual" -> "Manual"
    else -> words(raw)
}

/** "owner" -> "Owner", "read_only" -> "Read only". */
fun userRoleLabel(raw: String): String = words(raw)

/** Alert states as the web shows them: "firing" -> "Firing". */
fun alertStateLabel(raw: String): String = words(raw)

/** Port apply state: "failed: timeout" keeps its detail, with a capital first letter. */
fun portApplyLabel(raw: String): String = words(raw)

/** "House B4", or "" when the house has no number. The one place the word "House" is put in front of a number. */
fun houseText(unit: String): String = unit.trim().takeIf { it.isNotEmpty() }?.let { "House $it" }.orEmpty()

/**
 * A Kenyan phone number grouped for reading: "254712345678" or "0712345678" -> "0712 345 678".
 * Anything that is not a Kenyan mobile number is returned as typed (trimmed).
 */
fun formatPhone(raw: String): String {
    val t = raw.trim()
    val digits = t.filter { it.isDigit() }
    val local = when {
        digits.length == 12 && digits.startsWith("254") -> "0" + digits.substring(3)
        digits.length == 10 && digits.startsWith("0") -> digits
        digits.length == 9 && (digits.startsWith("7") || digits.startsWith("1")) -> "0$digits"
        else -> return t
    }
    return "${local.substring(0, 4)} ${local.substring(4, 7)} ${local.substring(7)}"
}

/** "1 house" / "3 houses": English plural for places that only have a [String] to build (view models, tests). */
fun countLabel(n: Int, one: String, many: String = one + "s"): String = "$n ${if (n == 1) one else many}"

private fun words(raw: String): String =
    raw.trim().replace('_', ' ').replaceFirstChar { it.uppercase() }

/** Card title on customer detail: "Apartment  ·  House N", plus the floor when there is one. */
fun accountCardTitle(a: ke.co.ftw.ftnadmin.domain.Account): String {
    val place = listOfNotNull(
        a.apartment?.name?.trim()?.takeIf { it.isNotEmpty() },
        houseText(a.unitLabel).takeIf { it.isNotEmpty() },
    ).joinToString("  ·  ")
    val floor = a.floor.trim().takeIf { it.isNotEmpty() }?.let { "floor $it" }
    return listOfNotNull(place.takeIf { it.isNotEmpty() }, floor).joinToString(", ")
}

/** Top-bar title for a customer: the tenant's name, else the house number, else [fallback]. */
fun customerBarTitle(a: ke.co.ftw.ftnadmin.domain.Account?, fallback: String): String = a?.headline?.ifBlank { null } ?: fallback

/** The lease status words: bound, waiting, none. */
fun leaseLabel(raw: String): String = when (raw) {
    "bound" -> "Bound"
    "waiting" -> "Waiting for an address"
    "none", "" -> "None"
    else -> words(raw)
}
