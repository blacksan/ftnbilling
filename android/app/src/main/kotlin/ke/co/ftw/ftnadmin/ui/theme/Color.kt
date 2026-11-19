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
)

/** Logo colours. */
object FtnColors {
    val Orange = Color(0xFFFF8E1C)
    val Navy = Color(0xFF001432)
}

val FastTechNetBrand = Brand(
    name = "Fast Tech Net",
    light = lightColorScheme(
        primary = Color(0xFF8F4E00),
        onPrimary = Color(0xFFFFFFFF),
        primaryContainer = Color(0xFFFFDCC2),
        onPrimaryContainer = Color(0xFF2E1500),
        secondary = Color(0xFF2A4A7F),
        onSecondary = Color(0xFFFFFFFF),
        secondaryContainer = Color(0xFFD7E2FF),
        onSecondaryContainer = Color(0xFF001A42),
        tertiary = Color(0xFF006A62),
        onTertiary = Color(0xFFFFFFFF),
        tertiaryContainer = Color(0xFF9EF2E6),
        onTertiaryContainer = Color(0xFF00201D),
        error = Color(0xFFBA1A1A),
        onError = Color(0xFFFFFFFF),
        errorContainer = Color(0xFFFFDAD6),
        onErrorContainer = Color(0xFF410002),
        background = Color(0xFFFFF8F5),
        onBackground = Color(0xFF221A14),
        surface = Color(0xFFFFF8F5),
        onSurface = Color(0xFF221A14),
        surfaceVariant = Color(0xFFF3DED1),
        onSurfaceVariant = Color(0xFF52443A),
        surfaceContainerLowest = Color(0xFFFFFFFF),
        surfaceContainerLow = Color(0xFFFFF1EA),
        surfaceContainer = Color(0xFFFAEBE3),
        surfaceContainerHigh = Color(0xFFF4E5DD),
        surfaceContainerHighest = Color(0xFFEEE0D8),
        outline = Color(0xFF857468),
        outlineVariant = Color(0xFFD7C3B6),
        inverseSurface = Color(0xFF382E28),
        inverseOnSurface = Color(0xFFFFEDE3),
        inversePrimary = Color(0xFFFFB77B),
        scrim = Color(0xFF000000),
    ),
    dark = darkColorScheme(
        primary = Color(0xFFFFB77B),
        onPrimary = Color(0xFF4C2700),
        primaryContainer = Color(0xFFFF8E1C),
        onPrimaryContainer = Color(0xFF2E1500),
        secondary = Color(0xFFAEC6FF),
        onSecondary = Color(0xFF0B2F63),
        secondaryContainer = Color(0xFF26467B),
        onSecondaryContainer = Color(0xFFD7E2FF),
        tertiary = Color(0xFF82D5CA),
        onTertiary = Color(0xFF003732),
        tertiaryContainer = Color(0xFF005049),
        onTertiaryContainer = Color(0xFF9EF2E6),
        error = Color(0xFFFFB4AB),
        onError = Color(0xFF690005),
        errorContainer = Color(0xFF93000A),
        onErrorContainer = Color(0xFFFFDAD6),
        background = Color(0xFF00102A),
        onBackground = Color(0xFFE3E6F0),
        surface = Color(0xFF00102A),
        onSurface = Color(0xFFE3E6F0),
        surfaceVariant = Color(0xFF3A3F4C),
        onSurfaceVariant = Color(0xFFC3C7D4),
        surfaceContainerLowest = Color(0xFF000A1D),
        surfaceContainerLow = Color(0xFF001432),
        surfaceContainer = Color(0xFF071B3B),
        surfaceContainerHigh = Color(0xFF132646),
        surfaceContainerHighest = Color(0xFF1E3152),
        outline = Color(0xFF8D919E),
        outlineVariant = Color(0xFF434753),
        inverseSurface = Color(0xFFE3E6F0),
        inverseOnSurface = Color(0xFF1C2333),
        inversePrimary = Color(0xFF8F4E00),
        scrim = Color(0xFF000000),
    ),
    extended = ExtendedColors(
        success = Color(0xFF1B6D2F),
        onSuccess = Color.White,
        successContainer = Color(0xFFA6F5A9),
        onSuccessContainer = Color(0xFF002108),
        warning = Color(0xFF7A5900),
        onWarning = Color.White,
        warningContainer = Color(0xFFFFDF9E),
        onWarningContainer = Color(0xFF261A00),
    ),
    extendedDark = ExtendedColors(
        success = Color(0xFF8BD88F),
        onSuccess = Color(0xFF003914),
        successContainer = Color(0xFF005320),
        onSuccessContainer = Color(0xFFA6F5A9),
        warning = Color(0xFFF6BE36),
        onWarning = Color(0xFF402D00),
        warningContainer = Color(0xFF5C4200),
        onWarningContainer = Color(0xFFFFDF9E),
    ),
)
