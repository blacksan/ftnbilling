package ke.co.ftw.ftnadmin.ui.common

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.FilterChip
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import androidx.compose.material3.Surface
import androidx.compose.runtime.Composable
import androidx.compose.ui.tooling.preview.Preview
import ke.co.ftw.ftnadmin.ui.theme.AppTheme

// Check these at font scale 1.0 and 1.3 in Android Studio: nothing may wrap letter by letter.

@Composable
private fun ChipsSample() {
    AppTheme {
        Surface {
            ChipRow {
                listOf("All", "Active", "Expired", "Suspended", "New").forEach {
                    FilterChip(selected = it == "All", onClick = {}, label = { ChipLabel(it) })
                }
            }
        }
    }
}

@Preview(widthDp = 360, fontScale = 1.0f, showBackground = true)
@Composable
private fun ChipsNormal() = ChipsSample()

@Preview(widthDp = 360, fontScale = 1.3f, showBackground = true)
@Composable
private fun ChipsLarge() = ChipsSample()

@Composable
private fun CardsSample() {
    AppTheme {
        Surface {
            Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
                StatCard("Throughput", "123.4", Modifier.fillMaxWidth(), unit = "Mbps", sub = "Up 22.1 Mbps", tone = CardTone.Brand, spark = listOf(1.0, 3.0, 2.0, 5.0, 4.0))
                StatCard("Online flats", "27", Modifier.fillMaxWidth(), unit = "of 40", tone = CardTone.Feature)
            }
        }
    }
}

@Preview(widthDp = 360, fontScale = 1.0f, showBackground = true)
@Composable
private fun CardsNormal() = CardsSample()

@Preview(widthDp = 360, fontScale = 1.5f, showBackground = true)
@Composable
private fun CardsLarge() = CardsSample()
