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
    /** Yellow fill for warning badges and bars: always pair it with dark text and an icon. */
    val warningFill: Color = Color(0xFFF5B800),
)

/**
 * Signal Indigo palette (direction A). Same hex values as the web tokens in static/app.css:
 * indigo 50-900 ramp, cool slate neutrals, no orange anywhere. Dark primary is tone 300.
 */
object SignalIndigo {
    val Indigo50 = Color(0xFFF0F1FB)
    val Indigo100 = Color(0xFFDFE1F6)
    val Indigo200 = Color(0xFFC0C4EC)
    val Indigo300 = Color(0xFF9AA0DE)
    val Indigo400 = Color(0xFF7479CE)
    val Indigo500 = Color(0xFF5558BA)
    val Indigo600 = Color(0xFF4345A0)
    val Indigo700 = Color(0xFF363882)
    val Indigo800 = Color(0xFF2B2D68)
    val Indigo900 = Color(0xFF1F2049)
}

val FastTechNetBrand = Brand(
    name = "Fast Tech Net",
    light = lightColorScheme(
        primary = SignalIndigo.Indigo600, onPrimary = Color(0xFFFFFFFF),
        primaryContainer = SignalIndigo.Indigo100, onPrimaryContainer = SignalIndigo.Indigo900,
        secondary = Color(0xFF475569), onSecondary = Color(0xFFFFFFFF),
        secondaryContainer = Color(0xFFE2E8F0), onSecondaryContainer = Color(0xFF0F172A),
        tertiary = Color(0xFF1F5FBF), onTertiary = Color(0xFFFFFFFF),
        tertiaryContainer = Color(0xFFE8F0FC), onTertiaryContainer = Color(0xFF0B2A5C),
        error = Color(0xFFC0283D), onError = Color(0xFFFFFFFF),
        errorContainer = Color(0xFFFDECEF), onErrorContainer = Color(0xFF7A1424),
        background = Color(0xFFF8FAFC), onBackground = Color(0xFF0F172A),
        surface = Color(0xFFF8FAFC), onSurface = Color(0xFF0F172A),
        surfaceVariant = Color(0xFFE2E8F0), onSurfaceVariant = Color(0xFF475569),
        surfaceContainerLowest = Color(0xFFFFFFFF), surfaceContainerLow = Color(0xFFFFFFFF),
        surfaceContainer = Color(0xFFF1F5F9), surfaceContainerHigh = Color(0xFFE9EEF5),
        surfaceContainerHighest = Color(0xFFE2E8F0),
        outline = Color(0xFF7C8BA1), outlineVariant = Color(0xFFE2E8F0),
        inverseSurface = Color(0xFF0F172A), inverseOnSurface = Color(0xFFF1F5F9),
        inversePrimary = SignalIndigo.Indigo300, scrim = Color(0xFF000000),
    ),
    dark = darkColorScheme(
        primary = SignalIndigo.Indigo300, onPrimary = SignalIndigo.Indigo900,
        primaryContainer = SignalIndigo.Indigo700, onPrimaryContainer = SignalIndigo.Indigo100,
        secondary = Color(0xFF94A3B8), onSecondary = Color(0xFF0F172A),
        secondaryContainer = Color(0xFF26314A), onSecondaryContainer = Color(0xFFE2E8F0),
        tertiary = Color(0xFF7FB0FF), onTertiary = Color(0xFF0B2A5C),
        tertiaryContainer = Color(0xFF14284A), onTertiaryContainer = Color(0xFFD6E4FF),
        error = Color(0xFFFF7A8A), onError = Color(0xFF4A0A16),
        errorContainer = Color(0xFF3D1A22), onErrorContainer = Color(0xFFFFD9DE),
        background = Color(0xFF0E1420), onBackground = Color(0xFFF1F5F9),
        surface = Color(0xFF0E1420), onSurface = Color(0xFFF1F5F9),
        surfaceVariant = Color(0xFF26314A), onSurfaceVariant = Color(0xFF94A3B8),
        surfaceContainerLowest = Color(0xFF151C2B), surfaceContainerLow = Color(0xFF151C2B),
        surfaceContainer = Color(0xFF1C2538), surfaceContainerHigh = Color(0xFF26314A),
        surfaceContainerHighest = Color(0xFF2E3A55),
        outline = Color(0xFF64748B), outlineVariant = Color(0xFF26314A),
        inverseSurface = Color(0xFFF1F5F9), inverseOnSurface = Color(0xFF0F172A),
        inversePrimary = SignalIndigo.Indigo600, scrim = Color(0xFF000000),
    ),
    extended = ExtendedColors(
        success = Color(0xFF1E7A4C), onSuccess = Color.White, successContainer = Color(0xFFE7F6EE), onSuccessContainer = Color(0xFF14532D),
        warning = Color(0xFF7A5800), onWarning = Color.White, warningContainer = Color(0xFFFFF6D6), onWarningContainer = Color(0xFF4A3500),
    ),
    extendedDark = ExtendedColors(
        success = Color(0xFF4CC38A), onSuccess = Color(0xFF04301B), successContainer = Color(0xFF123327), onSuccessContainer = Color(0xFFBFEBD4),
        warning = Color(0xFFF5C842), onWarning = Color(0xFF2A2000), warningContainer = Color(0xFF3A2E08), onWarningContainer = Color(0xFFFFE9A0),
    ),
)
