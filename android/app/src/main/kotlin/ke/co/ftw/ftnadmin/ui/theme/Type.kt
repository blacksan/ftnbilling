package ke.co.ftw.ftnadmin.ui.theme

import androidx.compose.material3.Typography
import androidx.compose.ui.text.ExperimentalTextApi
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.text.font.Font
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontVariation
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.sp
import ke.co.ftw.ftnadmin.R

/** Geist (SIL OFL 1.1) variable fonts in res/font; minSdk 26 supports variable axes. */
@OptIn(ExperimentalTextApi::class)
private fun variable(res: Int, weight: FontWeight) =
    Font(res, weight, variationSettings = FontVariation.Settings(FontVariation.weight(weight.weight)))

val Geist = FontFamily(
    listOf(FontWeight.Normal, FontWeight.Medium, FontWeight.SemiBold, FontWeight.Bold)
        .map { variable(R.font.geist_variable, it) },
)

/** Machine identifiers only: MAC, IP, receipt codes. */
val GeistMono = FontFamily(
    listOf(FontWeight.Normal, FontWeight.Medium, FontWeight.SemiBold)
        .map { variable(R.font.geist_mono_variable, it) },
)

/** Tabular figures so amounts and counts line up. */
private const val Tnum = "tnum"

private fun style(size: Int, line: Int, weight: FontWeight, tracking: Double = 0.0) = TextStyle(
    fontFamily = Geist,
    fontWeight = weight,
    fontSize = size.sp,
    lineHeight = line.sp,
    letterSpacing = tracking.sp,
    fontFeatureSettings = Tnum,
)

/** Web scale 12/13/14/16/20/24/32 mapped onto the M3 roles. */
val AppTypography = Typography(
    displayLarge = style(32, 40, FontWeight.SemiBold, -0.5),
    displayMedium = style(32, 40, FontWeight.SemiBold, -0.5),
    displaySmall = style(24, 32, FontWeight.SemiBold, -0.3),
    headlineLarge = style(32, 40, FontWeight.SemiBold, -0.5),
    headlineMedium = style(24, 32, FontWeight.SemiBold, -0.3),
    headlineSmall = style(20, 28, FontWeight.SemiBold, -0.2),
    titleLarge = style(20, 28, FontWeight.SemiBold, -0.2),
    titleMedium = style(16, 24, FontWeight.SemiBold),
    titleSmall = style(14, 20, FontWeight.Medium),
    bodyLarge = style(16, 24, FontWeight.Normal),
    bodyMedium = style(14, 20, FontWeight.Normal),
    bodySmall = style(13, 18, FontWeight.Normal),
    labelLarge = style(14, 20, FontWeight.Medium),
    labelMedium = style(13, 16, FontWeight.Medium),
    labelSmall = style(12, 16, FontWeight.Medium),
)
