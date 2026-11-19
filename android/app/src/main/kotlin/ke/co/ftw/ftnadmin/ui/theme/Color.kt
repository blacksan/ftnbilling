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
    /** Solid yellow marker fill (#F5B800 family) with dark text; the only place yellow is used as a fill. */
    val warningFill: Color,
    val onWarningFill: Color,
)

/**
 * Direction B "Meridian Blue". One blue (ramp below), cool slate neutrals, a fixed semantic set and no orange.
 * The same hex values drive the web tokens in internal/web/static/app.css.
 */
object MeridianBlue {
    val Blue50 = Color(0xFFEEF4FC)
    val Blue100 = Color(0xFFD9E6F8)
    val Blue200 = Color(0xFFB5CDF1)
    val Blue300 = Color(0xFF86ACE6)
    val Blue400 = Color(0xFF5588D8)
    val Blue500 = Color(0xFF306BC5)
    val Blue600 = Color(0xFF1F55AA)
    val Blue700 = Color(0xFF1A4488)
    val Blue800 = Color(0xFF163769)
    val Blue900 = Color(0xFF112849)
    val Slate50 = Color(0xFFF8FAFC)
    val Slate100 = Color(0xFFF1F5F9)
    val Slate200 = Color(0xFFE2E8F0)
    val Slate300 = Color(0xFFCBD5E1)
    val Slate400 = Color(0xFF94A3B8)
    val Slate500 = Color(0xFF64748B)
    val Slate600 = Color(0xFF475569)
    val Slate700 = Color(0xFF334155)
    val Slate800 = Color(0xFF1E293B)
    val Slate900 = Color(0xFF0F172A)
}

val FastTechNetBrand = Brand(
    name = "Fast Tech Net",
    light = lightColorScheme(
        primary = Color(0xFF1F55AA),
        onPrimary = Color(0xFFFFFFFF),
        primaryContainer = Color(0xFFD9E6F8),
        onPrimaryContainer = Color(0xFF112849),
        secondary = Color(0xFF475569),
        onSecondary = Color(0xFFFFFFFF),
        secondaryContainer = Color(0xFFE2E8F0),
        onSecondaryContainer = Color(0xFF0F172A),
        tertiary = Color(0xFF166C85),
        onTertiary = Color(0xFFFFFFFF),
        tertiaryContainer = Color(0xFFCFEBF2),
        onTertiaryContainer = Color(0xFF0C333E),
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
        surfaceContainerLow = Color(0xFFF4F7FB),
        surfaceContainer = Color(0xFFF1F5F9),
        surfaceContainerHigh = Color(0xFFE9EEF5),
        surfaceContainerHighest = Color(0xFFE2E8F0),
        outline = Color(0xFF64748B),
        outlineVariant = Color(0xFFD5DDE8),
        inverseSurface = Color(0xFF0F172A),
        inverseOnSurface = Color(0xFFF1F5F9),
        inversePrimary = Color(0xFF86ACE6),
        scrim = Color(0xFF000000),
    ),
    dark = darkColorScheme(
        primary = Color(0xFF86ACE6),
        onPrimary = Color(0xFF112849),
        primaryContainer = Color(0xFF163769),
        onPrimaryContainer = Color(0xFFD9E6F8),
        secondary = Color(0xFF94A3B8),
        onSecondary = Color(0xFF0F172A),
        secondaryContainer = Color(0xFF243047),
        onSecondaryContainer = Color(0xFFE2E8F0),
        tertiary = Color(0xFF66BCD2),
        onTertiary = Color(0xFF0C333E),
        tertiaryContainer = Color(0xFF135769),
        onTertiaryContainer = Color(0xFFCFEBF2),
        error = Color(0xFFFF7A8A),
        onError = Color(0xFF1A0A0E),
        errorContainer = Color(0xFF3A141C),
        onErrorContainer = Color(0xFFFFD9DE),
        background = Color(0xFF0E1420),
        onBackground = Color(0xFFF1F5F9),
        surface = Color(0xFF0E1420),
        onSurface = Color(0xFFF1F5F9),
        surfaceVariant = Color(0xFF243047),
        onSurfaceVariant = Color(0xFF94A3B8),
        surfaceContainerLowest = Color(0xFF0A0F18),
        surfaceContainerLow = Color(0xFF111826),
        surfaceContainer = Color(0xFF151C2B),
        surfaceContainerHigh = Color(0xFF1B2438),
        surfaceContainerHighest = Color(0xFF243047),
        outline = Color(0xFF7C8DA9),
        outlineVariant = Color(0xFF243047),
        inverseSurface = Color(0xFFF1F5F9),
        inverseOnSurface = Color(0xFF0F172A),
        inversePrimary = Color(0xFF1F55AA),
        scrim = Color(0xFF000000),
    ),
    extended = ExtendedColors(
        success = Color(0xFF1E7A4C),
        onSuccess = Color.White,
        successContainer = Color(0xFFE7F6EE),
        onSuccessContainer = Color(0xFF0E3B25),
        warning = Color(0xFF7A5800),
        onWarning = Color.White,
        warningContainer = Color(0xFFFFF6D6),
        onWarningContainer = Color(0xFF3D2C00),
        warningFill = Color(0xFFF5B800),
        onWarningFill = Color(0xFF0F172A),
    ),
    extendedDark = ExtendedColors(
        success = Color(0xFF4CC38A),
        onSuccess = Color(0xFF062A17),
        successContainer = Color(0xFF10301F),
        onSuccessContainer = Color(0xFFBDEBD3),
        warning = Color(0xFFF5C842),
        onWarning = Color(0xFF1A1400),
        warningContainer = Color(0xFF3A2C00),
        onWarningContainer = Color(0xFFFBE8A6),
        warningFill = Color(0xFFF5C842),
        onWarningFill = Color(0xFF1A1400),
    ),
)
