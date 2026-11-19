package ke.co.ftw.ftnadmin.ui.common

import java.time.Duration
import java.time.Instant
import java.time.ZoneId
import java.time.format.DateTimeFormatter
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

fun formatBps(bps: Long): String {
    if (bps < 1000) return "$bps bps"
    val units = arrayOf("kbps", "Mbps", "Gbps")
    var v = bps / 1000.0
    var i = 0
    while (v >= 1000 && i < units.lastIndex) {
        v /= 1000
        i++
    }
    return String.format(Locale.US, "%.1f %s", v, units[i])
}

fun formatKes(amount: Long): String = String.format(Locale.US, "KES %,d", amount)

fun parseInstant(s: String?): Instant? = if (s.isNullOrBlank()) null else try {
    Instant.parse(s)
} catch (_: Exception) {
    try {
        java.time.OffsetDateTime.parse(s).toInstant()
    } catch (_: Exception) {
        null
    }
}

private val DATE_TIME = DateTimeFormatter.ofPattern("d MMM yyyy HH:mm", Locale.ENGLISH).withZone(ZoneId.of("Africa/Nairobi"))
private val DATE = DateTimeFormatter.ofPattern("d MMM yyyy", Locale.ENGLISH).withZone(ZoneId.of("Africa/Nairobi"))

/** Nairobi local time, or "-" when absent. */
fun formatDateTime(s: String?): String = parseInstant(s)?.let(DATE_TIME::format) ?: "-"

fun formatDate(s: String?): String = parseInstant(s)?.let(DATE::format) ?: "-"

/** "12 s ago", "3 min ago", "2 h ago", "4 d ago", or "never". */
fun formatAge(s: String?, now: Instant = Instant.now()): String {
    val t = parseInstant(s) ?: return "never"
    val secs = Duration.between(t, now).seconds.coerceAtLeast(0)
    return when {
        secs < 60 -> "$secs s ago"
        secs < 3600 -> "${secs / 60} min ago"
        secs < 86_400 -> "${secs / 3600} h ago"
        else -> "${secs / 86_400} d ago"
    }
}

/** "in 3 d", "expired 2 h ago" style text for an expiry. */
fun formatExpiry(s: String?, now: Instant = Instant.now()): String {
    val t = parseInstant(s) ?: return "No expiry"
    return if (t.isAfter(now)) {
        val secs = Duration.between(now, t).seconds
        val rel = when {
            secs < 3600 -> "${secs / 60} min"
            secs < 86_400 -> "${secs / 3600} h"
            else -> "${secs / 86_400} d"
        }
        "${formatDateTime(s)} (in $rel)"
    } else {
        "${formatDateTime(s)} (${formatAge(s, now)})"
    }
}

/** Number and unit apart, for big-number cards: 1_234_000 -> ("1.2", "Mbps"). */
fun splitBps(bps: Long): Pair<String, String> {
    if (bps < 1000) return bps.toString() to "bps"
    val units = arrayOf("kbps", "Mbps", "Gbps")
    var v = bps / 1000.0
    var i = 0
    while (v >= 1000 && i < units.lastIndex) {
        v /= 1000
        i++
    }
    return String.format(Locale.US, if (v >= 100) "%.0f" else "%.1f", v) to units[i]
}

/** "3d 4h", "5h 12m" or "42m". */
fun formatUptime(seconds: Long): String {
    val s = seconds.coerceAtLeast(0)
    val d = s / 86_400
    val h = (s % 86_400) / 3600
    val m = (s % 3600) / 60
    return when {
        d > 0 -> "${d}d ${h}h"
        h > 0 -> "${h}h ${m}m"
        else -> "${m}m"
    }
}

fun formatPct(pct: Double): String = String.format(Locale.US, if (pct >= 10) "%.0f" else "%.1f", pct)
