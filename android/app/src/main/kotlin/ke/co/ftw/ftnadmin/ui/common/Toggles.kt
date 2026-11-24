package ke.co.ftw.ftnadmin.ui.common

import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.selection.selectable
import androidx.compose.foundation.selection.toggleable
import androidx.compose.material3.Checkbox
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.RadioButton
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.unit.dp

/** A checkbox whose whole row (48 dp tall at least) toggles, with one focus stop and the label as its name. */
@Composable
fun CheckRow(checked: Boolean, onChange: (Boolean) -> Unit, label: String, modifier: Modifier = Modifier, enabled: Boolean = true) {
    Row(
        modifier.fillMaxWidth().heightIn(min = 48.dp)
            .toggleable(value = checked, enabled = enabled, role = Role.Checkbox, onValueChange = onChange)
            .padding(horizontal = 4.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Checkbox(checked = checked, onCheckedChange = null, enabled = enabled)
        Spacer(Modifier.width(12.dp))
        Text(label, style = MaterialTheme.typography.bodyLarge, color = MaterialTheme.colorScheme.onSurface, modifier = Modifier.weight(1f))
    }
}

/** A switch whose whole row toggles; the label wraps and the switch stays at the end. */
@Composable
fun SwitchRow(checked: Boolean, onChange: (Boolean) -> Unit, label: String, modifier: Modifier = Modifier, enabled: Boolean = true) {
    Row(
        modifier.fillMaxWidth().heightIn(min = 48.dp)
            .toggleable(value = checked, enabled = enabled, role = Role.Switch, onValueChange = onChange)
            .padding(horizontal = 4.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Text(label, style = MaterialTheme.typography.bodyLarge, color = MaterialTheme.colorScheme.onSurface, modifier = Modifier.weight(1f))
        Spacer(Modifier.width(12.dp))
        Switch(checked = checked, onCheckedChange = null, enabled = enabled)
    }
}

/** A radio row whose whole row selects. Put several in a Column with `Modifier.selectableGroup()`. */
@Composable
fun RadioRow(selected: Boolean, onSelect: () -> Unit, modifier: Modifier = Modifier, content: @Composable androidx.compose.foundation.layout.RowScope.() -> Unit) {
    Row(
        modifier.fillMaxWidth().heightIn(min = 48.dp)
            .selectable(selected = selected, role = Role.RadioButton, onClick = onSelect)
            .padding(horizontal = 4.dp, vertical = 4.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        RadioButton(selected = selected, onClick = null)
        Spacer(Modifier.width(12.dp))
        content()
    }
}

/** A status dot centred on the first line of text beside it, whatever the font scale (the line height scales with it). */
@Composable
fun FirstLineDot(color: androidx.compose.ui.graphics.Color, modifier: Modifier = Modifier, style: androidx.compose.ui.text.TextStyle = MaterialTheme.typography.bodyMedium) {
    val h = with(androidx.compose.ui.platform.LocalDensity.current) { style.lineHeight.toDp() }
    androidx.compose.foundation.layout.Box(modifier.heightIn(min = h).height(h), contentAlignment = Alignment.Center) {
        androidx.compose.material3.Surface(Modifier.size(12.dp), shape = androidx.compose.foundation.shape.CircleShape, color = color) {}
    }
}
