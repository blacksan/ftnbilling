package ke.co.ftw.ftnadmin.domain

/** Why a MAC the operator typed was refused. */
enum class MacProblem(val message: String) {
    Empty("Enter the ONT's MAC address."),
    BadCharacters("A MAC address only has the digits 0-9 and the letters A-F."),
    BadLength("A MAC address has 12 hex digits, like AA:BB:CC:DD:EE:FF."),
    AllZero("00:00:00:00:00:00 is not a real device address."),
    Broadcast("FF:FF:FF:FF:FF:FF is the broadcast address, not a device."),
    Multicast("This is a group (multicast) address. An ONT's address has an even first byte."),
}

sealed interface MacResult {
    data class Valid(val mac: String) : MacResult

    data class Invalid(val problem: MacProblem) : MacResult
}

/**
 * Normalises and validates MACs the way the server does: any common spelling in, `AA:BB:CC:DD:EE:FF` out.
 * Accepts `:` `-` `.` and space separators in any grouping, or none.
 */
object MacAddress {
    fun check(input: String): MacResult {
        val trimmed = input.trim()
        if (trimmed.isEmpty()) return MacResult.Invalid(MacProblem.Empty)
        if (trimmed.any { !(it.isHexDigit() || it in SEPARATORS) }) return MacResult.Invalid(MacProblem.BadCharacters)
        val hex = trimmed.filter { it.isHexDigit() }.uppercase()
        if (hex.length != 12) return MacResult.Invalid(MacProblem.BadLength)
        if (hex.all { it == '0' }) return MacResult.Invalid(MacProblem.AllZero)
        if (hex.all { it == 'F' }) return MacResult.Invalid(MacProblem.Broadcast)
        if ((hex.substring(0, 2).toInt(16) and 1) == 1) return MacResult.Invalid(MacProblem.Multicast)
        return MacResult.Valid(hex.chunked(2).joinToString(":"))
    }

    /** Canonical form or null. */
    fun normalize(input: String): String? = (check(input) as? MacResult.Valid)?.mac

    fun isValid(input: String): Boolean = check(input) is MacResult.Valid

    /** Error text for a text field, or null when the input is fine or still empty. */
    fun errorFor(input: String): String? = when (val r = check(input)) {
        is MacResult.Valid -> null
        is MacResult.Invalid -> if (r.problem == MacProblem.Empty) null else r.problem.message
    }

    private val SEPARATORS = charArrayOf(':', '-', '.', ' ')

    private fun Char.isHexDigit() = this in '0'..'9' || this in 'a'..'f' || this in 'A'..'F'
}
