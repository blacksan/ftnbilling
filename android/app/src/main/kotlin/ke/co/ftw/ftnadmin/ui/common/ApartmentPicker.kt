package ke.co.ftw.ftnadmin.ui.common

import androidx.compose.foundation.BorderStroke
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.text.KeyboardActions
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.ArrowDropDown
import androidx.compose.material.icons.filled.Check
import androidx.compose.material.icons.filled.Search
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.ModalBottomSheet
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Text
import androidx.compose.material3.rememberModalBottomSheetState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import ke.co.ftw.ftnadmin.domain.APARTMENT_SEARCH_FROM
import ke.co.ftw.ftnadmin.domain.Apartment
import ke.co.ftw.ftnadmin.domain.matching

/**
 * Read-only field that shows the chosen apartment and opens the picker on tap. The whole field is the
 * touch target (a read-only text field swallows taps itself, so a transparent layer sits on top).
 */
@Composable
fun ApartmentField(
    selected: Apartment?,
    onClick: () -> Unit,
    modifier: Modifier = Modifier,
    enabled: Boolean = true,
) {
    Box(modifier.fillMaxWidth()) {
        OutlinedTextField(
            value = selected?.name.orEmpty(),
            onValueChange = {},
            readOnly = true,
            singleLine = true,
            label = { Text("Apartment") },
            placeholder = { Text("Choose an apartment") },
            supportingText = selected?.address?.takeIf { it.isNotBlank() }?.let { a -> { Text(a, maxLines = 1, overflow = TextOverflow.Ellipsis) } },
            trailingIcon = { Icon(Icons.Default.ArrowDropDown, contentDescription = null) },
            enabled = enabled,
            modifier = Modifier.fillMaxWidth(),
        )
        Box(Modifier.matchParentSize().clickable(enabled = enabled, role = Role.Button, onClick = onClick))
    }
}

/**
 * Bottom sheet listing all [apartments]: name, address, subscribers and routers. A search box appears when there
 * are more than [APARTMENT_SEARCH_FROM] of them. Tapping a row calls [onSelect] and closes the sheet.
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun ApartmentPickerSheet(
    apartments: List<Apartment>,
    selectedId: String?,
    onSelect: (Apartment) -> Unit,
    onDismiss: () -> Unit,
) {
    var query by remember { mutableStateOf("") }
    val shown = apartments.matching(query)
    ModalBottomSheet(onDismissRequest = onDismiss, sheetState = rememberModalBottomSheetState(skipPartiallyExpanded = true)) {
        Column(Modifier.padding(horizontal = 16.dp).navigationBarsPadding(), verticalArrangement = Arrangement.spacedBy(12.dp)) {
            Text("Choose an apartment", style = MaterialTheme.typography.titleLarge, fontWeight = FontWeight.ExtraBold)
            if (apartments.size > APARTMENT_SEARCH_FROM) {
                OutlinedTextField(
                    value = query,
                    onValueChange = { query = it },
                    singleLine = true,
                    placeholder = { Text("Search by name, address or router", maxLines = 1) },
                    leadingIcon = { Icon(Icons.Default.Search, contentDescription = null) },
                    keyboardOptions = KeyboardOptions(imeAction = ImeAction.Search),
                    keyboardActions = KeyboardActions(onSearch = { shown.singleOrNull()?.let(onSelect) }),
                    modifier = Modifier.fillMaxWidth(),
                )
            }
            when {
                apartments.isEmpty() -> Text("No apartments yet.", color = MaterialTheme.colorScheme.onSurfaceVariant)
                shown.isEmpty() -> Text("No apartment matches.", color = MaterialTheme.colorScheme.onSurfaceVariant)
                else -> LazyColumn(Modifier.heightIn(max = 520.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                    items(shown, key = { it.id }) { a -> ApartmentRow(a, selected = a.id == selectedId, onClick = { onSelect(a) }) }
                }
            }
        }
    }
}

@Composable
private fun ApartmentRow(a: Apartment, selected: Boolean, onClick: () -> Unit) {
    val scheme = MaterialTheme.colorScheme
    Card(
        onClick = onClick,
        modifier = Modifier.fillMaxWidth(),
        colors = CardDefaults.cardColors(
            containerColor = if (selected) scheme.primaryContainer else scheme.surfaceContainerHigh,
            contentColor = if (selected) scheme.onPrimaryContainer else scheme.onSurface,
        ),
        border = if (selected) BorderStroke(2.dp, scheme.primary) else null,
    ) {
        Row(Modifier.padding(14.dp).fillMaxWidth(), verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(12.dp)) {
            Column(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(2.dp)) {
                Text(a.name, style = MaterialTheme.typography.titleMedium, fontWeight = FontWeight.ExtraBold, maxLines = 2, overflow = TextOverflow.Ellipsis)
                if (a.address.isNotBlank()) Text(a.address, style = MaterialTheme.typography.bodyMedium, maxLines = 2, overflow = TextOverflow.Ellipsis)
                Text(a.pickerSummary, style = MaterialTheme.typography.bodySmall, color = if (selected) scheme.onPrimaryContainer else scheme.onSurfaceVariant, maxLines = 2, overflow = TextOverflow.Ellipsis)
            }
            if (selected) Icon(Icons.Default.Check, contentDescription = "Selected")
        }
    }
}
