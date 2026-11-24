package ke.co.ftw.ftnadmin.ui.common

import androidx.compose.foundation.selection.selectableGroup
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.res.pluralStringResource
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import ke.co.ftw.ftnadmin.R
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
            label = { Text(stringResource(R.string.search_accounts)) },
            supportingText = { Text(stringResource(R.string.search_accounts_hint)) },
            singleLine = true,
            modifier = Modifier.fillMaxWidth(),
        )
        val shown = if (suggested != null && query.isBlank()) listOf(suggested) + results.filter { it.id != suggested.id } else results
        // No scroll area of its own: the sheet or screen around it scrolls (one scroll container per surface). The list is capped
        // and the search box narrows it.
        Column(Modifier.selectableGroup()) {
            shown.take(MAX_SHOWN).forEach { a ->
                RadioRow(selected = a.id == selectedId, onSelect = { onSelect(a) }, modifier = Modifier.padding(vertical = 2.dp)) {
                    Column(Modifier.weight(1f)) {
                        Text(
                            "${a.unitLabel.ifBlank { a.code }}${if (a.id == suggested?.id) "  " + stringResource(R.string.suggested) else ""}",
                            style = MaterialTheme.typography.bodyLarge,
                            color = MaterialTheme.colorScheme.onSurface,
                        )
                        Text(
                            listOf(a.tenant, formatPhone(a.phone)).filter { it.isNotBlank() }.joinToString("  |  "),
                            style = MaterialTheme.typography.bodySmall,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                        MonoText(
                            listOf(a.code, a.mac.ifBlank { stringResource(R.string.no_mac_yet) }).filter { it.isNotBlank() }.joinToString("  |  "),
                            style = MaterialTheme.typography.bodySmall,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                    }
                }
            }
            if (shown.size > MAX_SHOWN) {
                Text(
                    pluralStringResource(R.plurals.more_matches_narrow, shown.size - MAX_SHOWN, shown.size - MAX_SHOWN),
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.padding(8.dp),
                )
            }
            if (shown.isEmpty() && !loading) {
                Text(
                    error ?: stringResource(R.string.no_matching_accounts),
                    style = MaterialTheme.typography.bodyMedium,
                    color = if (error != null) MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.padding(8.dp),
                )
            }
        }
    }
}

private const val MAX_SHOWN = 12
