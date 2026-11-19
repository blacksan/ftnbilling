package ke.co.ftw.ftnadmin.ui.theme

import androidx.compose.material3.Typography
import androidx.compose.ui.text.ExperimentalTextApi
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.text.font.Font
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontVariation
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.TextUnit
import androidx.compose.ui.unit.em
import androidx.compose.ui.unit.sp
import ke.co.ftw.ftnadmin.R

/**
 * Inter (UI and numbers, tabular figures) and JetBrains Mono (ids, IPs, MAC addresses, M-Pesa codes),
 * both bundled as variable fonts in res/font under the SIL Open Font License 1.1 (see assets/licenses).
 */
@OptIn(ExperimentalTextApi::class)
private fun variable(resId: Int, weight: FontWeight) =
    Font(resId, weight, variationSettings = FontVariation.Settings(FontVariation.weight(weight.weight)))

val InterFamily = FontFamily(
    variable(R.font.inter_variable, FontWeight.Normal),
    variable(R.font.inter_variable, FontWeight.Medium),
    variable(R.font.inter_variable, FontWeight.SemiBold),
    variable(R.font.inter_variable, FontWeight.Bold),
    variable(R.font.inter_variable, FontWeight.ExtraBold),
)

val MonoFamily = FontFamily(
    variable(R.font.jetbrains_mono_variable, FontWeight.Normal),
    variable(R.font.jetbrains_mono_variable, FontWeight.Medium),
    variable(R.font.jetbrains_mono_variable, FontWeight.SemiBold),
    variable(R.font.jetbrains_mono_variable, FontWeight.Bold),
)

/** Tabular figures so amounts and counters line up in lists. */
private const val Tnum = "tnum"

private fun inter(size: Int, line: Int, weight: FontWeight, tracking: TextUnit = 0.sp) = TextStyle(
    fontFamily = InterFamily,
    fontWeight = weight,
    fontSize = size.sp,
    lineHeight = line.sp,
    letterSpacing = tracking,
    fontFeatureSettings = Tnum,
)

/** One scale on the web and here: 12 / 13 / 14 / 16 / 20 / 24 / 32. */
val AppTypography = Typography(
    displayLarge = inter(32, 40, FontWeight.SemiBold, (-0.02).em),
    displayMedium = inter(32, 40, FontWeight.SemiBold, (-0.02).em),
    displaySmall = inter(32, 40, FontWeight.SemiBold, (-0.02).em),
    headlineLarge = inter(32, 40, FontWeight.SemiBold, (-0.02).em),
    headlineMedium = inter(24, 32, FontWeight.SemiBold, (-0.015).em),
    headlineSmall = inter(20, 28, FontWeight.SemiBold, (-0.01).em),
    titleLarge = inter(20, 28, FontWeight.SemiBold, (-0.01).em),
    titleMedium = inter(16, 24, FontWeight.Medium),
    titleSmall = inter(14, 20, FontWeight.Medium),
    bodyLarge = inter(16, 24, FontWeight.Normal),
    bodyMedium = inter(14, 20, FontWeight.Normal),
    bodySmall = inter(13, 18, FontWeight.Normal),
    labelLarge = inter(14, 20, FontWeight.SemiBold),
    labelMedium = inter(13, 16, FontWeight.Medium),
    labelSmall = inter(12, 16, FontWeight.Medium),
)

/** Machine identifiers: ids, IPs, MAC addresses, M-Pesa receipt codes. */
val MonoStyle = TextStyle(
    fontFamily = MonoFamily,
    fontWeight = FontWeight.Normal,
    fontSize = 13.sp,
    lineHeight = 18.sp,
)
