package ke.co.ftw.ftnadmin.ui.common

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.RadioButton
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import ke.co.ftw.ftnadmin.domain.Account
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.delay

/**
 * Searchable single-choice list of accounts. [search] gets the typed text (empty = default list)
 * and is debounced; [suggestedId] is listed first and preselected via [selectedId] by the caller.
 */
@Composable
fun AccountPicker(
    selectedId: String?,
    onSelect: (Account) -> Unit,
    search: suspend (String) -> List<Account>,
    modifier: Modifier = Modifier,
    suggested: Account? = null,
) {
    var query by remember { mutableStateOf("") }
    var results by remember { mutableStateOf<List<Account>>(emptyList()) }
    var loading by remember { mutableStateOf(true) }
    var error by remember { mutableStateOf<String?>(null) }

    LaunchedEffect(query) {
        if (query.isNotEmpty()) delay(300)
        loading = true
        try {
            results = search(query.trim())
            error = null
        } catch (e: CancellationException) {
            throw e
        } catch (e: Exception) {
            error = e.userMessage()
        }
        loading = false
    }

    Column(modifier, verticalArrangement = Arrangement.spacedBy(8.dp)) {
        OutlinedTextField(
            value = query,
            onValueChange = { query = it },
            label = { Text("Search flat, tenant or phone") },
            singleLine = true,
            modifier = Modifier.fillMaxWidth(),
        )
        val shown = if (suggested != null && query.isBlank()) listOf(suggested) + results.filter { it.id != suggested.id } else results
        Column(Modifier.heightIn(max = 240.dp).verticalScroll(rememberScrollState())) {
            shown.forEach { a ->
                Row(
                    Modifier.fillMaxWidth().clickable { onSelect(a) }.padding(vertical = 6.dp),
                    verticalAlignment = Alignment.CenterVertically,
                ) {
                    RadioButton(selected = a.id == selectedId, onClick = null)
                    Column(Modifier.padding(start = 12.dp)) {
                        Text(
                            "Flat ${a.flat}${if (a.id == suggested?.id) "  (suggested)" else ""}",
                            style = MaterialTheme.typography.bodyLarge,
                            color = MaterialTheme.colorScheme.onSurface,
                        )
                        Text(
                            listOf(a.tenant, a.phone, a.code, a.mac.ifBlank { "no MAC yet" }).filter { it.isNotBlank() }.joinToString("  |  "),
                            style = MaterialTheme.typography.bodySmall,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                    }
                }
            }
            if (shown.isEmpty() && !loading) {
                Text(
                    error ?: "No matching accounts.",
                    style = MaterialTheme.typography.bodySmall,
                    color = if (error != null) MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.padding(8.dp),
                )
            }
        }
    }
}
