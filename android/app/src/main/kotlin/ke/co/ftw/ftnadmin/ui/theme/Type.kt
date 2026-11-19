package ke.co.ftw.ftnadmin.ui.theme

import androidx.compose.material3.Typography
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.text.font.Font
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontVariation
import androidx.compose.ui.text.font.FontWeight
import ke.co.ftw.ftnadmin.R

/** Source Sans 3 (variable, SIL OFL 1.1): compact and legible in dense lists; ships tabular figures. */
val SourceSans3 = FontFamily(
    listOf(
        FontWeight.Normal, FontWeight.Medium, FontWeight.SemiBold, FontWeight.Bold,
    ).map { w ->
        Font(
            R.font.source_sans_3_variable,
            weight = w,
            variationSettings = FontVariation.Settings(FontVariation.weight(w.weight)),
        )
    },
)

/** IBM Plex Mono (SIL OFL 1.1) for MAC addresses, IPs, receipt codes. */
val PlexMono = FontFamily(
    Font(R.font.ibm_plex_mono_regular, FontWeight.Normal),
    Font(R.font.ibm_plex_mono_medium, FontWeight.Medium),
    Font(R.font.ibm_plex_mono_semibold, FontWeight.SemiBold),
)

private const val Tabular = "tnum, lnum"

private fun TextStyle.sans(weight: FontWeight? = null) =
    copy(fontFamily = SourceSans3, fontWeight = weight ?: fontWeight, fontFeatureSettings = Tabular)

private val Base = Typography()

// Same M3 type scale; Source Sans 3 everywhere, tabular figures on, weights capped at SemiBold.
val AppTypography = Typography(
    displayLarge = Base.displayLarge.sans(FontWeight.SemiBold),
    displayMedium = Base.displayMedium.sans(FontWeight.SemiBold),
    displaySmall = Base.displaySmall.sans(FontWeight.SemiBold),
    headlineLarge = Base.headlineLarge.sans(FontWeight.SemiBold),
    headlineMedium = Base.headlineMedium.sans(FontWeight.SemiBold),
    headlineSmall = Base.headlineSmall.sans(FontWeight.SemiBold),
    titleLarge = Base.titleLarge.sans(FontWeight.SemiBold),
    titleMedium = Base.titleMedium.sans(FontWeight.SemiBold),
    titleSmall = Base.titleSmall.sans(FontWeight.SemiBold),
    bodyLarge = Base.bodyLarge.sans(),
    bodyMedium = Base.bodyMedium.sans(),
    bodySmall = Base.bodySmall.sans(),
    labelLarge = Base.labelLarge.sans(FontWeight.SemiBold),
    labelMedium = Base.labelMedium.sans(FontWeight.Medium),
    labelSmall = Base.labelSmall.sans(FontWeight.Medium),
)

/** Monospace style for identifiers. */
val MonoStyle = TextStyle(fontFamily = PlexMono, fontWeight = FontWeight.Normal, fontFeatureSettings = "zero")
