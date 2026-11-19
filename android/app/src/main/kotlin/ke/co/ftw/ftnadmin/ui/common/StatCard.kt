package ke.co.ftw.ftnadmin.ui.common

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
import androidx.compose.foundation.BorderStroke
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
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
 * Card tones. Direction D keeps every card neutral (surface + hairline border); the tone only colours the
 * small accent (icon, sparkline, and the figure for Bad) when it carries status meaning: Good, Warn, Bad, Info.
 * Feature, Brand, Teal and Neutral all render as plain neutral cards so existing call sites keep compiling.
 */
enum class CardTone { Feature, Brand, Info, Teal, Good, Warn, Bad, Neutral }

private data class ToneColors(val container: Color, val content: Color, val accent: Color, val value: Color)

@Composable
private fun CardTone.colors(): ToneColors {
    val s = MaterialTheme.colorScheme
    val x = AppTheme.extended
    val neutral = ToneColors(s.surfaceContainerLowest, s.onSurface, s.onSurfaceVariant, s.onSurface)
    return when (this) {
        CardTone.Info -> neutral.copy(accent = s.tertiary)
        CardTone.Good -> neutral.copy(accent = x.success)
        CardTone.Warn -> neutral.copy(accent = x.warning)
        CardTone.Bad -> neutral.copy(accent = s.error, value = s.error)
        else -> neutral
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
    val colors = CardDefaults.cardColors(containerColor = c.container, contentColor = c.content)
    val border = BorderStroke(1.dp, MaterialTheme.colorScheme.outlineVariant)
    val desc = listOf(label, "$value $unit".trim(), sub).filter { it.isNotBlank() }.joinToString(", ")
    val body: @Composable () -> Unit = {
        Column(Modifier.padding(12.dp).fillMaxWidth(), verticalArrangement = Arrangement.spacedBy(4.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                if (icon != null) {
                    Icon(icon, contentDescription = null, tint = c.accent, modifier = Modifier.size(18.dp))
                    Spacer(Modifier.size(8.dp))
                }
                Text(
                    label,
                    style = MaterialTheme.typography.labelLarge,
                    fontWeight = FontWeight.Medium,
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
                    color = c.value,
                    maxLines = 1,
                    softWrap = false,
                )
                if (unit.isNotBlank()) {
                    Text(
                        unit,
                        style = MaterialTheme.typography.titleSmall,
                        fontWeight = FontWeight.Medium,
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
                Sparkline(spark, color = c.accent, height = 32.dp)
            }
        }
    }
    val m = modifier.semantics(mergeDescendants = true) { contentDescription = desc }
    if (onClick != null) Card(onClick = onClick, modifier = m, colors = colors, border = border) { body() } else Card(modifier = m, colors = colors, border = border) { body() }
}
