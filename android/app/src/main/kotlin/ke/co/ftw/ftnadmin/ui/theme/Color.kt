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
    /** Yellow fill for warning chips and icons; always paired with [onWarningFill] and a label. */
    val warningFill: Color,
    val onWarningFill: Color,
)

/** Direction C "Lattice Teal": the 50-900 brand ramp, shared with the web tokens (tokens.css). */
object FtnColors {
    val Teal50 = Color(0xFFEDF7F7)
    val Teal100 = Color(0xFFD2EDEC)
    val Teal200 = Color(0xFFA6DAD8)
    val Teal300 = Color(0xFF6FC0BE)
    val Teal400 = Color(0xFF3FA2A0)
    val Teal500 = Color(0xFF238683)
    val Teal600 = Color(0xFF1A6D6B)
    val Teal700 = Color(0xFF175756)
    val Teal800 = Color(0xFF144645)
    val Teal900 = Color(0xFF0F3433)
}

/** Fixed brand scheme: dynamic colour stays off unless the user turns it on in Settings. */
val FastTechNetBrand = Brand(
    name = "Fast Tech Net",
    light = lightColorScheme(
        primary = FtnColors.Teal600,
        onPrimary = Color(0xFFFFFFFF),
        primaryContainer = FtnColors.Teal100,
        onPrimaryContainer = FtnColors.Teal900,
        secondary = Color(0xFF475569),
        onSecondary = Color(0xFFFFFFFF),
        secondaryContainer = Color(0xFFE2E8F0),
        onSecondaryContainer = Color(0xFF1E293B),
        tertiary = Color(0xFF1F5FBF),
        onTertiary = Color(0xFFFFFFFF),
        tertiaryContainer = Color(0xFFE8F0FD),
        onTertiaryContainer = Color(0xFF0B2A5E),
        error = Color(0xFFC0283D),
        onError = Color(0xFFFFFFFF),
        errorContainer = Color(0xFFFDECEF),
        onErrorContainer = Color(0xFF7A0F20),
        background = Color(0xFFF8FAFC),
        onBackground = Color(0xFF0F172A),
        surface = Color(0xFFF8FAFC),
        onSurface = Color(0xFF0F172A),
        surfaceVariant = Color(0xFFE2E8F0),
        onSurfaceVariant = Color(0xFF475569),
        surfaceContainerLowest = Color(0xFFFFFFFF),
        surfaceContainerLow = Color(0xFFFBFCFD),
        surfaceContainer = Color(0xFFF1F5F9),
        surfaceContainerHigh = Color(0xFFEAEFF5),
        surfaceContainerHighest = Color(0xFFE2E8F0),
        outline = Color(0xFF64748B),
        outlineVariant = Color(0xFFE2E8F0),
        inverseSurface = Color(0xFF1E293B),
        inverseOnSurface = Color(0xFFF1F5F9),
        inversePrimary = FtnColors.Teal300,
        scrim = Color(0xFF000000),
    ),
    dark = darkColorScheme(
        primary = FtnColors.Teal300,
        onPrimary = FtnColors.Teal900,
        primaryContainer = FtnColors.Teal700,
        onPrimaryContainer = FtnColors.Teal100,
        secondary = Color(0xFFCBD5E1),
        onSecondary = Color(0xFF1E293B),
        secondaryContainer = Color(0xFF243047),
        onSecondaryContainer = Color(0xFFE2E8F0),
        tertiary = Color(0xFF7FB0FF),
        onTertiary = Color(0xFF0B2A5E),
        tertiaryContainer = Color(0xFF14284A),
        onTertiaryContainer = Color(0xFFD6E4FF),
        error = Color(0xFFFF7A8A),
        onError = Color(0xFF4A0A14),
        errorContainer = Color(0xFF3F1620),
        onErrorContainer = Color(0xFFFFD9DE),
        background = Color(0xFF0E1420),
        onBackground = Color(0xFFF1F5F9),
        surface = Color(0xFF0E1420),
        onSurface = Color(0xFFF1F5F9),
        surfaceVariant = Color(0xFF243047),
        onSurfaceVariant = Color(0xFF94A3B8),
        surfaceContainerLowest = Color(0xFF0A0F1A),
        surfaceContainerLow = Color(0xFF111826),
        surfaceContainer = Color(0xFF151C2B),
        surfaceContainerHigh = Color(0xFF1B2436),
        surfaceContainerHighest = Color(0xFF243047),
        outline = Color(0xFF7587A5),
        outlineVariant = Color(0xFF243047),
        inverseSurface = Color(0xFFF1F5F9),
        inverseOnSurface = Color(0xFF0F172A),
        inversePrimary = FtnColors.Teal600,
        scrim = Color(0xFF000000),
    ),
    extended = ExtendedColors(
        success = Color(0xFF1E7A4C),
        onSuccess = Color.White,
        successContainer = Color(0xFFE7F6EE),
        onSuccessContainer = Color(0xFF14532D),
        warning = Color(0xFF7A5800),
        onWarning = Color.White,
        warningContainer = Color(0xFFFFF6D6),
        onWarningContainer = Color(0xFF4A3500),
        warningFill = Color(0xFFF5B800),
        onWarningFill = Color(0xFF0F172A),
    ),
    extendedDark = ExtendedColors(
        success = Color(0xFF4CC38A),
        onSuccess = Color(0xFF06281A),
        successContainer = Color(0xFF10301F),
        onSuccessContainer = Color(0xFFBFF0D6),
        warning = Color(0xFFF5C842),
        onWarning = Color(0xFF3A2E08),
        warningContainer = Color(0xFF3A2E08),
        onWarningContainer = Color(0xFFFDE9A6),
        warningFill = Color(0xFFF5B800),
        onWarningFill = Color(0xFF0F172A),
    ),
)
