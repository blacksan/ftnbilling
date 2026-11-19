package ke.co.ftw.ftnadmin.ui.common

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.RadioButton
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import ke.co.ftw.ftnadmin.domain.ServicePackage

/** Radio list of packages: name, speed, price. */
@Composable
fun PackagePicker(
    packages: List<ServicePackage>,
    selectedId: String?,
    onSelect: (String) -> Unit,
    modifier: Modifier = Modifier,
) {
    if (packages.isEmpty()) {
        Text("No packages available.", color = MaterialTheme.colorScheme.onSurfaceVariant, style = MaterialTheme.typography.bodyMedium)
        return
    }
    Column(modifier.heightIn(max = 280.dp).verticalScroll(rememberScrollState())) {
        packages.forEach { p ->
            Row(
                Modifier.fillMaxWidth().clickable { onSelect(p.id) }.padding(vertical = 6.dp),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                RadioButton(selected = p.id == selectedId, onClick = null)
                Column(Modifier.padding(start = 12.dp)) {
                    Text(p.name, style = MaterialTheme.typography.bodyLarge, color = MaterialTheme.colorScheme.onSurface)
                    Text(
                        "${p.speedMbps} Mbps  |  ${formatKes(p.priceKes)}  |  ${formatDuration(p.durationMin)}",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
            }
        }
    }
}

fun formatDuration(min: Int): String = when {
    min <= 0 -> "no limit"
    min % 1440 == 0 -> "${min / 1440} d"
    min % 60 == 0 -> "${min / 60} h"
    else -> "$min min"
}
