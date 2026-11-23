package ke.co.ftw.ftnadmin.domain

/** The Wi-Fi name and password rules the server enforces, checked here first so the owner sees the reason at once. */
object WifiRules {
    const val SSID_MAX_BYTES = 32
    const val PASS_MIN = 8
    const val PASS_MAX = 63

    /** Why [ssid] cannot be used, or null. 1 to 32 bytes of printable text, no space at the ends. */
    fun ssidError(ssid: String): String? = when {
        ssid.isEmpty() -> "Enter a Wi-Fi name."
        ssid.toByteArray(Charsets.UTF_8).size > SSID_MAX_BYTES -> "The Wi-Fi name can be at most 32 bytes (fewer letters if it has accents or emoji)."
        ssid.any { it.code < 0x20 || it.code == 0x7f } -> "The Wi-Fi name has a control character."
        ssid.trim() != ssid -> "The Wi-Fi name cannot start or end with a space."
        else -> null
    }

    /** Why [pass] cannot be used as a WPA2 password, or null. 8 to 63 printable English characters. */
    fun passwordError(pass: String): String? = when {
        pass.length < PASS_MIN -> "The password needs at least 8 characters."
        pass.length > PASS_MAX -> "The password can be at most 63 characters."
        pass.any { it.code < 0x20 || it.code > 0x7e } -> "Use English letters, digits and symbols only."
        else -> null
    }
}
