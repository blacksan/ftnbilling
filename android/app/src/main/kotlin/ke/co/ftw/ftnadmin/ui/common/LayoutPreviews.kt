package ke.co.ftw.ftnadmin.ui.common

import androidx.compose.material3.FilterChip
import androidx.compose.material3.Surface
import androidx.compose.runtime.Composable
import androidx.compose.ui.tooling.preview.Preview
import ke.co.ftw.ftnadmin.ui.dashboard.AttentionItem
import ke.co.ftw.ftnadmin.ui.dashboard.AttentionTiles
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
private fun TilesSample() {
    AppTheme {
        Surface {
            AttentionTiles(
                listOf(
                    AttentionItem("New devices", "New devices", 3, {}),
                    AttentionItem("Unmatched", "Unmatched payments", 0, {}),
                    AttentionItem("Alerts", "Open alerts", 12, {}),
                ),
            )
        }
    }
}

@Preview(widthDp = 360, fontScale = 1.0f, showBackground = true)
@Composable
private fun TilesNormal() = TilesSample()

@Preview(widthDp = 360, fontScale = 1.5f, showBackground = true)
@Composable
private fun TilesLarge() = TilesSample()
