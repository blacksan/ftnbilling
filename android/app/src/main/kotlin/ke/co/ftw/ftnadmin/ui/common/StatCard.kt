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
 * Stat card tones. Cards are neutral (surface, hairline border, number first). Colour appears only on the
 * small icon chip and the sparkline, and only when it means something: [Warn] and [Bad]. Every other tone
 * is kept so existing call sites compile, and renders as the brand-tinted neutral card.
 */
enum class CardTone { Feature, Brand, Info, Teal, Good, Warn, Bad, Neutral }

private data class ToneColors(val chip: Color, val onChip: Color, val line: Color)

@Composable
private fun CardTone.colors(): ToneColors {
    val s = MaterialTheme.colorScheme
    val x = AppTheme.extended
    return when (this) {
        CardTone.Warn -> ToneColors(x.warningContainer, x.warning, x.warning)
        CardTone.Bad -> ToneColors(s.errorContainer, s.error, s.error)
        else -> ToneColors(s.primaryContainer, s.primary, s.primary)
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
        Column(Modifier.padding(16.dp).fillMaxWidth(), verticalArrangement = Arrangement.spacedBy(4.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                if (icon != null) {
                    Surface(shape = MaterialTheme.shapes.extraSmall, color = c.chip, contentColor = c.onChip, modifier = Modifier.size(32.dp)) {
                        Icon(icon, contentDescription = null, modifier = Modifier.padding(7.dp))
                    }
                    Spacer(Modifier.size(12.dp))
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
                    style = MaterialTheme.typography.headlineLarge,
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
                Text(sub, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 2, overflow = TextOverflow.Ellipsis)
            }
            if (spark.size >= 2) {
                Spacer(Modifier.height(2.dp))
                Sparkline(spark, color = c.line, height = 28.dp)
            }
        }
    }
    val m = modifier.semantics(mergeDescendants = true) { contentDescription = desc }
    if (onClick != null) Card(onClick = onClick, modifier = m, colors = colors, border = border) { body() } else Card(modifier = m, colors = colors, border = border) { body() }
}
