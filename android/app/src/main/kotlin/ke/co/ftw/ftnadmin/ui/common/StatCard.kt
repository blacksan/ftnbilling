package ke.co.ftw.ftnadmin.ui.common

import androidx.compose.foundation.BorderStroke
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import ke.co.ftw.ftnadmin.ui.theme.AppTheme

/**
 * Stat cards are neutral (surface, hairline border, number + label). Colour appears only where it means
 * something: Good, Warn, Bad and Info tint the icon marker and the sub line. Feature, Brand, Teal and
 * Neutral all render the same neutral card; the sparkline then uses the brand blue.
 */
enum class CardTone { Feature, Brand, Info, Teal, Good, Warn, Bad, Neutral }

/** [marker] and [onMarker] colour the icon chip, [text] the sub line, [spark] the sparkline. */
private data class ToneColors(val marker: Color, val onMarker: Color, val text: Color, val spark: Color)

@Composable
private fun CardTone.colors(): ToneColors {
    val s = MaterialTheme.colorScheme
    val x = AppTheme.extended
    return when (this) {
        CardTone.Good -> ToneColors(x.successContainer, x.success, x.success, x.success)
        CardTone.Warn -> ToneColors(x.warningFill, x.onWarningFill, x.warning, x.warning)
        CardTone.Bad -> ToneColors(s.errorContainer, s.error, s.error, s.error)
        CardTone.Info -> ToneColors(s.tertiaryContainer, s.tertiary, s.tertiary, s.tertiary)
        CardTone.Feature, CardTone.Brand, CardTone.Teal, CardTone.Neutral ->
            ToneColors(s.surfaceContainer, s.onSurfaceVariant, s.onSurfaceVariant, s.primary)
    }
}

/**
 * Dashboard metric card: small label, a bold number with its unit, an optional sub line and sparkline.
 * The number and unit sit in a [FlowRow] so a large font scale wraps the unit instead of squeezing text.
 */
@OptIn(ExperimentalLayoutApi::class)
@Composable
fun StatCard(
    label: String,
    value: String,
    modifier: Modifier = Modifier,
    unit: String = "",
    sub: String = "",
    tone: CardTone = CardTone.Neutral,
    icon: ImageVector? = null,
    spark: List<Double> = emptyList(),
    onClick: (() -> Unit)? = null,
) {
    val c = tone.colors()
    val colors = CardDefaults.cardColors(
        containerColor = MaterialTheme.colorScheme.surfaceContainerLowest,
        contentColor = MaterialTheme.colorScheme.onSurface,
    )
    val border = BorderStroke(1.dp, MaterialTheme.colorScheme.outlineVariant)
    val desc = listOf(label, "$value $unit".trim(), sub).filter { it.isNotBlank() }.joinToString(", ")
    val body: @Composable () -> Unit = {
        Column(Modifier.padding(12.dp).fillMaxWidth(), verticalArrangement = Arrangement.spacedBy(4.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                if (icon != null) {
                    Surface(shape = MaterialTheme.shapes.extraSmall, color = c.marker, contentColor = c.onMarker, modifier = Modifier.size(24.dp)) {
                        Icon(icon, contentDescription = null, modifier = Modifier.padding(4.dp))
                    }
                    Spacer(Modifier.size(8.dp))
                }
                Text(
                    label,
                    style = MaterialTheme.typography.labelMedium,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    maxLines = 1,
                    overflow = TextOverflow.Ellipsis,
                    modifier = Modifier.weight(1f),
                )
            }
            FlowRow(horizontalArrangement = Arrangement.spacedBy(4.dp), verticalArrangement = Arrangement.Bottom) {
                Text(
                    value,
                    style = MaterialTheme.typography.headlineMedium,
                    fontWeight = FontWeight.SemiBold,
                    maxLines = 1,
                    softWrap = false,
                )
                if (unit.isNotBlank()) {
                    Text(
                        unit,
                        style = MaterialTheme.typography.titleSmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        maxLines = 1,
                        softWrap = false,
                        modifier = Modifier.padding(bottom = 4.dp),
                    )
                }
            }
            if (sub.isNotBlank()) {
                Text(sub, style = MaterialTheme.typography.bodySmall, color = c.text, maxLines = 2, overflow = TextOverflow.Ellipsis)
            }
            if (spark.size >= 2) {
                Spacer(Modifier.height(2.dp))
                Sparkline(spark, color = c.spark, height = 28.dp)
            }
        }
    }
    val m = modifier.semantics(mergeDescendants = true) { contentDescription = desc }
    if (onClick != null) Card(onClick = onClick, modifier = m, colors = colors, border = border) { body() } else Card(modifier = m, colors = colors, border = border) { body() }
}
