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
import androidx.compose.foundation.shape.CircleShape
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
import androidx.compose.ui.graphics.luminance
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import ke.co.ftw.ftnadmin.ui.theme.AppTheme
import ke.co.ftw.ftnadmin.ui.theme.FtnColors

/**
 * Card colour families. Each maps onto existing theme roles (container + on-container, with a solid
 * accent for the icon chip), so light and dark both stay legible without new colour tokens.
 */
enum class CardTone { Feature, Brand, Info, Teal, Good, Warn, Bad, Neutral }

private data class ToneColors(val container: Color, val content: Color, val accent: Color, val onAccent: Color)

@Composable
private fun CardTone.colors(): ToneColors {
    val s = MaterialTheme.colorScheme
    val x = AppTheme.extended
    val dark = s.background.luminance() < 0.5f
    return when (this) {
        CardTone.Feature ->
            if (dark) ToneColors(s.primaryContainer, s.onPrimaryContainer, s.onPrimaryContainer, s.primaryContainer)
            else ToneColors(FtnColors.Navy, Color.White, FtnColors.Orange, FtnColors.Navy)
        CardTone.Brand -> ToneColors(s.primaryContainer, s.onPrimaryContainer, s.primary, s.onPrimary)
        CardTone.Info -> ToneColors(s.secondaryContainer, s.onSecondaryContainer, s.secondary, s.onSecondary)
        CardTone.Teal -> ToneColors(s.tertiaryContainer, s.onTertiaryContainer, s.tertiary, s.onTertiary)
        CardTone.Good -> ToneColors(x.successContainer, x.onSuccessContainer, x.success, x.onSuccess)
        CardTone.Warn -> ToneColors(x.warningContainer, x.onWarningContainer, x.warning, x.onWarning)
        CardTone.Bad -> ToneColors(s.errorContainer, s.onErrorContainer, s.error, s.onError)
        CardTone.Neutral -> ToneColors(s.surfaceContainerHigh, s.onSurface, s.outline, s.surface)
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
    val desc = listOf(label, "$value $unit".trim(), sub).filter { it.isNotBlank() }.joinToString(", ")
    val body: @Composable () -> Unit = {
        Column(Modifier.padding(14.dp).fillMaxWidth(), verticalArrangement = Arrangement.spacedBy(4.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                if (icon != null) {
                    Surface(shape = CircleShape, color = c.accent, contentColor = c.onAccent, modifier = Modifier.size(28.dp)) {
                        Icon(icon, contentDescription = null, modifier = Modifier.padding(5.dp))
                    }
                    Spacer(Modifier.size(8.dp))
                }
                Text(
                    label,
                    style = MaterialTheme.typography.labelLarge,
                    fontWeight = FontWeight.Bold,
                    maxLines = 1,
                    overflow = TextOverflow.Ellipsis,
                    modifier = Modifier.weight(1f),
                )
            }
            FlowRow(horizontalArrangement = Arrangement.spacedBy(4.dp), verticalArrangement = Arrangement.Bottom) {
                Text(
                    value,
                    style = MaterialTheme.typography.headlineMedium,
                    fontWeight = FontWeight.ExtraBold,
                    maxLines = 1,
                    softWrap = false,
                )
                if (unit.isNotBlank()) {
                    Text(
                        unit,
                        style = MaterialTheme.typography.titleSmall,
                        fontWeight = FontWeight.Bold,
                        maxLines = 1,
                        softWrap = false,
                        modifier = Modifier.padding(bottom = 4.dp),
                    )
                }
            }
            if (sub.isNotBlank()) {
                Text(sub, style = MaterialTheme.typography.bodySmall, maxLines = 2, overflow = TextOverflow.Ellipsis)
            }
            if (spark.size >= 2) {
                Spacer(Modifier.height(2.dp))
                Sparkline(spark, color = c.accent, height = 32.dp)
            }
        }
    }
    val m = modifier.semantics(mergeDescendants = true) { contentDescription = desc }
    if (onClick != null) Card(onClick = onClick, modifier = m, colors = colors) { body() } else Card(modifier = m, colors = colors) { body() }
}
