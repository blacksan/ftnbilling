package ke.co.ftw.ftnadmin.ui.theme

import androidx.compose.material3.ColorScheme
import androidx.compose.material3.darkColorScheme
import androidx.compose.material3.lightColorScheme
import androidx.compose.runtime.Immutable
import androidx.compose.ui.graphics.Color

/** A named light/dark colour pair plus the status colours Material 3 does not define. */
@Immutable
data class Brand(
    val name: String,
    val light: ColorScheme,
    val dark: ColorScheme,
    val extended: ExtendedColors,
    val extendedDark: ExtendedColors,
)

/**
 * Status roles Material 3 has no slot for. `warning` is the dark yellow-brown used for text and icons on
 * surfaces; `warningFill` is the yellow fill (never orange) and `onWarningFill` the text that sits on it.
 */
@Immutable
data class ExtendedColors(
    val success: Color,
    val onSuccess: Color,
    val successContainer: Color,
    val onSuccessContainer: Color,
    val warning: Color,
    val onWarning: Color,
    val warningContainer: Color,
    val onWarningContainer: Color,
    val warningFill: Color,
    val onWarningFill: Color,
)

/** Logo colours (launcher icon and wordmark only; the interface itself uses the slate navy ramp below). */
object FtnColors {
    val Orange = Color(0xFFFF8E1C)
    val Navy = Color(0xFF001432)
}

/** Direction D "Slate Navy" ramp. 600 is the brand primary; dark theme uses tone 300 as primary. */
object SlateNavy {
    val N50 = Color(0xFFF2F5F8)
    val N100 = Color(0xFFE1E7EE)
    val N200 = Color(0xFFC3CEDB)
    val N300 = Color(0xFF9AAEC4)
    val N400 = Color(0xFF6F89A6)
    val N500 = Color(0xFF4F6C8C)
    val N600 = Color(0xFF3D5673)
    val N700 = Color(0xFF31455E)
    val N800 = Color(0xFF273748)
    val N900 = Color(0xFF1C2733)
}

val FastTechNetBrand = Brand(
    name = "Fast Tech Net",
    light = lightColorScheme(
        primary = Color(0xFF3D5673),
        onPrimary = Color(0xFFFFFFFF),
        primaryContainer = Color(0xFFE1E7EE),
        onPrimaryContainer = Color(0xFF1C2733),
        secondary = Color(0xFF475569),
        onSecondary = Color(0xFFFFFFFF),
        secondaryContainer = Color(0xFFE2E8F0),
        onSecondaryContainer = Color(0xFF1E293B),
        tertiary = Color(0xFF1F5FBF),
        onTertiary = Color(0xFFFFFFFF),
        tertiaryContainer = Color(0xFFE8F0FC),
        onTertiaryContainer = Color(0xFF0B2A5C),
        error = Color(0xFFC0283D),
        onError = Color(0xFFFFFFFF),
        errorContainer = Color(0xFFFDECEF),
        onErrorContainer = Color(0xFF7A1020),
        background = Color(0xFFF8FAFC),
        onBackground = Color(0xFF0F172A),
        surface = Color(0xFFF8FAFC),
        onSurface = Color(0xFF0F172A),
        surfaceVariant = Color(0xFFE2E8F0),
        onSurfaceVariant = Color(0xFF475569),
        surfaceContainerLowest = Color(0xFFFFFFFF),
        surfaceContainerLow = Color(0xFFF4F7FA),
        surfaceContainer = Color(0xFFF1F5F9),
        surfaceContainerHigh = Color(0xFFEAEFF5),
        surfaceContainerHighest = Color(0xFFE2E8F0),
        outline = Color(0xFF6B7A90),
        outlineVariant = Color(0xFFDCE3EC),
        inverseSurface = Color(0xFF1E293B),
        inverseOnSurface = Color(0xFFF1F5F9),
        inversePrimary = Color(0xFF9AAEC4),
        scrim = Color(0xFF000000),
    ),
    dark = darkColorScheme(
        primary = Color(0xFF9AAEC4),
        onPrimary = Color(0xFF1C2733),
        primaryContainer = Color(0xFF31455E),
        onPrimaryContainer = Color(0xFFE1E7EE),
        secondary = Color(0xFFB6C2D2),
        onSecondary = Color(0xFF1E293B),
        secondaryContainer = Color(0xFF334155),
        onSecondaryContainer = Color(0xFFE2E8F0),
        tertiary = Color(0xFF7FB0FF),
        onTertiary = Color(0xFF0B2A5C),
        tertiaryContainer = Color(0xFF14284A),
        onTertiaryContainer = Color(0xFFD6E6FF),
        error = Color(0xFFFF7A8A),
        onError = Color(0xFF4A0A14),
        errorContainer = Color(0xFF3A1620),
        onErrorContainer = Color(0xFFFFD9DE),
        background = Color(0xFF0E1420),
        onBackground = Color(0xFFF1F5F9),
        surface = Color(0xFF0E1420),
        onSurface = Color(0xFFF1F5F9),
        surfaceVariant = Color(0xFF243049),
        onSurfaceVariant = Color(0xFF94A3B8),
        surfaceContainerLowest = Color(0xFF0B1019),
        surfaceContainerLow = Color(0xFF121A28),
        surfaceContainer = Color(0xFF151C2B),
        surfaceContainerHigh = Color(0xFF1B2436),
        surfaceContainerHighest = Color(0xFF243049),
        outline = Color(0xFF7487A3),
        outlineVariant = Color(0xFF263147),
        inverseSurface = Color(0xFFF1F5F9),
        inverseOnSurface = Color(0xFF1E293B),
        inversePrimary = Color(0xFF3D5673),
        scrim = Color(0xFF000000),
    ),
    extended = ExtendedColors(
        success = Color(0xFF1E7A4C),
        onSuccess = Color(0xFFFFFFFF),
        successContainer = Color(0xFFE7F6EE),
        onSuccessContainer = Color(0xFF14532D),
        warning = Color(0xFF7A5800),
        onWarning = Color(0xFFFFFFFF),
        warningContainer = Color(0xFFFFF6D6),
        onWarningContainer = Color(0xFF7A5800),
        warningFill = Color(0xFFF5B800),
        onWarningFill = Color(0xFF3F2D00),
    ),
    extendedDark = ExtendedColors(
        success = Color(0xFF4CC38A),
        onSuccess = Color(0xFF06281A),
        successContainer = Color(0xFF12301F),
        onSuccessContainer = Color(0xFFBFEBD2),
        warning = Color(0xFFF5C842),
        onWarning = Color(0xFF3A2E08),
        warningContainer = Color(0xFF3A2E08),
        onWarningContainer = Color(0xFFF5C842),
        warningFill = Color(0xFFF5C842),
        onWarningFill = Color(0xFF3A2E08),
    ),
)
