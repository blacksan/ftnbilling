package ke.co.ftw.ftnadmin.ui.common

import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.selection.selectableGroup
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Modifier
import androidx.compose.ui.res.stringResource
import ke.co.ftw.ftnadmin.R
import ke.co.ftw.ftnadmin.domain.ServicePackage

/** Radio list of plans: name, speed, price. The surface around it scrolls; the whole row selects (48 dp). */
@Composable
fun PackagePicker(
    packages: List<ServicePackage>,
    selectedId: String?,
    onSelect: (String) -> Unit,
    modifier: Modifier = Modifier,
) {
    if (packages.isEmpty()) {
        Text(stringResource(R.string.no_plans_available), color = MaterialTheme.colorScheme.onSurfaceVariant, style = MaterialTheme.typography.bodyMedium)
        return
    }
    Column(modifier.selectableGroup()) {
        packages.forEach { p ->
            RadioRow(selected = p.id == selectedId, onSelect = { onSelect(p.id) }) {
                Column(Modifier.weight(1f)) {
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
