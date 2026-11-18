package ke.co.ftw.ftnadmin.ui.common

import java.util.Locale

fun formatBytes(bytes: Long): String {
    if (bytes < 1024) return "$bytes B"
    val units = arrayOf("KiB", "MiB", "GiB", "TiB")
    var value = bytes / 1024.0
    var i = 0
    while (value >= 1024 && i < units.lastIndex) {
        value /= 1024
        i++
    }
    return String.format(Locale.US, "%.1f %s", value, units[i])
}

/** "12 s ago", "3 min ago", or "never" when [epochMs] is 0. */
fun formatAge(epochMs: Long, nowMs: Long): String {
    if (epochMs <= 0) return "never"
    val s = ((nowMs - epochMs) / 1000).coerceAtLeast(0)
    return when {
        s < 60 -> "$s s ago"
        s < 3600 -> "${s / 60} min ago"
        else -> "${s / 3600} h ago"
    }
}

fun formatKes(amount: Long): String = String.format(Locale.US, "KES %,d", amount)
