package ke.co.ftw.ftnadmin.ui.theme

import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.darkColorScheme
import androidx.compose.material3.lightColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.ui.graphics.Color

private val Orange = Color(0xFFFF8E1C)
private val Navy = Color(0xFF001432)

private val LightColors = lightColorScheme(
    primary = Color(0xFFA85A00),
    onPrimary = Color.White,
    primaryContainer = Color(0xFFFFDCBE),
    onPrimaryContainer = Color(0xFF2C1600),
    secondary = Color(0xFF3A4B6E),
    onSecondary = Color.White,
    secondaryContainer = Color(0xFFD9E2FF),
    onSecondaryContainer = Navy,
    tertiary = Color(0xFF00629D),
    background = Color(0xFFFFFBF8),
    onBackground = Color(0xFF1D1B19),
    surface = Color(0xFFFFFBF8),
    onSurface = Color(0xFF1D1B19),
    surfaceVariant = Color(0xFFF2E0D0),
    onSurfaceVariant = Color(0xFF51443A),
)

private val DarkColors = darkColorScheme(
    primary = Orange,
    onPrimary = Navy,
    primaryContainer = Color(0xFF7A3F00),
    onPrimaryContainer = Color(0xFFFFDCBE),
    secondary = Color(0xFFB3C5F5),
    onSecondary = Navy,
    secondaryContainer = Color(0xFF1E3358),
    onSecondaryContainer = Color(0xFFD9E2FF),
    tertiary = Color(0xFF9DCAFF),
    background = Color(0xFF000C1F),
    onBackground = Color(0xFFE6E8F0),
    surface = Color(0xFF000C1F),
    onSurface = Color(0xFFE6E8F0),
    surfaceVariant = Color(0xFF14243F),
    onSurfaceVariant = Color(0xFFC3C9D8),
)

/** Material 3 scheme seeded from the bulb logo: #FF8E1C and #001432. No dynamic colour, to keep the brand. */
@Composable
fun FtnTheme(darkTheme: Boolean = isSystemInDarkTheme(), content: @Composable () -> Unit) {
    MaterialTheme(colorScheme = if (darkTheme) DarkColors else LightColors, content = content)
}
