package ke.co.ftw.ftnadmin.ui.theme

import androidx.compose.material3.Typography
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.text.font.Font
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.sp
import ke.co.ftw.ftnadmin.R

/** IBM Plex Sans (SIL OFL 1.1, bundled in res/font) for the UI. */
val PlexSans = FontFamily(
    Font(R.font.ibm_plex_sans_regular, FontWeight.Normal),
    Font(R.font.ibm_plex_sans_medium, FontWeight.Medium),
    Font(R.font.ibm_plex_sans_semibold, FontWeight.SemiBold),
    Font(R.font.ibm_plex_sans_semibold, FontWeight.Bold),
)

/** IBM Plex Mono for machine identifiers: MAC, IP, receipt codes. */
val PlexMono = FontFamily(
    Font(R.font.ibm_plex_mono_regular, FontWeight.Normal),
    Font(R.font.ibm_plex_mono_medium, FontWeight.Medium),
    Font(R.font.ibm_plex_mono_medium, FontWeight.SemiBold),
)

/** Monospace text style for codes, used as `Text(mac, style = MonoStyle)`. */
val MonoStyle = TextStyle(fontFamily = PlexMono, fontSize = 13.sp, lineHeight = 18.sp, fontFeatureSettings = "zero")

private fun sans(size: Int, line: Int, weight: FontWeight, tracking: Double = 0.0) = TextStyle(
    fontFamily = PlexSans,
    fontSize = size.sp,
    lineHeight = line.sp,
    fontWeight = weight,
    letterSpacing = tracking.sp,
    fontFeatureSettings = "tnum",
)

/** The shared scale: 12 / 13 / 14 / 16 / 20 / 24 / 32 sp, weights 400 / 500 / 600 only. */
val AppTypography = Typography(
    displayLarge = sans(32, 40, FontWeight.SemiBold),
    displayMedium = sans(32, 40, FontWeight.SemiBold),
    displaySmall = sans(24, 32, FontWeight.SemiBold),
    headlineLarge = sans(32, 40, FontWeight.SemiBold),
    headlineMedium = sans(24, 32, FontWeight.SemiBold),
    headlineSmall = sans(20, 28, FontWeight.SemiBold),
    titleLarge = sans(20, 28, FontWeight.SemiBold),
    titleMedium = sans(16, 24, FontWeight.Medium),
    titleSmall = sans(14, 20, FontWeight.Medium),
    bodyLarge = sans(16, 24, FontWeight.Normal),
    bodyMedium = sans(14, 20, FontWeight.Normal),
    bodySmall = sans(12, 16, FontWeight.Normal),
    labelLarge = sans(14, 20, FontWeight.Medium),
    labelMedium = sans(13, 16, FontWeight.Medium),
    labelSmall = sans(12, 16, FontWeight.Medium),
)
