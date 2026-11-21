package ke.co.ftw.ftnadmin.ui.apartments

import android.app.Activity
import android.content.ActivityNotFoundException
import android.content.Context
import android.content.Intent
import android.net.Uri
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Add
import androidx.compose.material.icons.filled.Clear
import androidx.compose.material.icons.filled.Place
import androidx.compose.material.icons.filled.Search
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.AssistChip
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.Checkbox
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.ExtendedFloatingActionButton
import androidx.compose.material3.FilledTonalButton
import androidx.compose.material3.FilterChip
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.pulltorefresh.PullToRefreshBox
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.LocalUriHandler
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import ke.co.ftw.ftnadmin.core.network.UserInfo
import ke.co.ftw.ftnadmin.domain.Apartment
import ke.co.ftw.ftnadmin.domain.ApartmentContact
import ke.co.ftw.ftnadmin.domain.GpsPoint
import ke.co.ftw.ftnadmin.ui.common.AppTopBar
import ke.co.ftw.ftnadmin.ui.common.CenteredLoading
import ke.co.ftw.ftnadmin.ui.common.ChipLabel
import ke.co.ftw.ftnadmin.ui.common.EmptyState
import ke.co.ftw.ftnadmin.ui.common.ErrorState
import ke.co.ftw.ftnadmin.ui.common.LabeledValue
import ke.co.ftw.ftnadmin.ui.common.Loadable
import ke.co.ftw.ftnadmin.ui.common.LocalContainer
import ke.co.ftw.ftnadmin.ui.common.SectionCard
import ke.co.ftw.ftnadmin.ui.common.StatusChip
import ke.co.ftw.ftnadmin.ui.common.Tone
import ke.co.ftw.ftnadmin.ui.common.vmFactory
import ke.co.ftw.ftnadmin.ui.customers.AccountRow

// ---- list ---------------------------------------------------------------------------------------

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun ApartmentsScreen(user: UserInfo, onBack: () -> Unit, onOpen: (String) -> Unit, onAdd: () -> Unit) {
    val container = LocalContainer.current
    val vm: ApartmentsViewModel = viewModel(factory = vmFactory { ApartmentsViewModel(container.apartments) })
    val ui by vm.state.collectAsStateWithLifecycle()
    val snack = remember { SnackbarHostState() }
    LaunchedEffect(ui.message) {
        ui.message?.let {
            snack.showSnackbar(it)
            vm.clearMessage()
        }
    }
    Scaffold(
        containerColor = MaterialTheme.colorScheme.background,
        topBar = { AppTopBar("Apartments", onBack = onBack) },
        snackbarHost = { SnackbarHost(snack) },
        floatingActionButton = {
            if (user.canWrite) {
                ExtendedFloatingActionButton(
                    onClick = onAdd,
                    icon = { Icon(Icons.Default.Add, contentDescription = null) },
                    text = { Text("New apartment", fontWeight = FontWeight.ExtraBold, maxLines = 1, softWrap = false) },
                    containerColor = MaterialTheme.colorScheme.primaryContainer,
                    contentColor = MaterialTheme.colorScheme.onPrimaryContainer,
                )
            }
        },
    ) { inner ->
        Column(Modifier.padding(inner).fillMaxSize()) {
            OutlinedTextField(
                value = ui.query,
                onValueChange = vm::onQuery,
                placeholder = { Text("Search apartment, address, caretaker or landlord") },
                leadingIcon = { Icon(Icons.Default.Search, contentDescription = null) },
                trailingIcon = {
                    if (ui.query.isNotEmpty()) {
                        IconButton(onClick = { vm.onQuery("") }) { Icon(Icons.Default.Clear, contentDescription = "Clear search") }
                    }
                },
                singleLine = true,
                modifier = Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 8.dp),
            )
            when (val items = ui.items) {
                Loadable.Loading -> CenteredLoading()
                is Loadable.Failed -> ErrorState(items.message, vm::retry)
                is Loadable.Ready -> PullToRefreshBox(isRefreshing = ui.refreshing, onRefresh = vm::refresh, modifier = Modifier.fillMaxSize()) {
                    if (items.value.isEmpty()) {
                        EmptyState(if (ui.query.isBlank()) "No apartments yet. Add an apartment, link its routers, then add customers to it with their house numbers." else "No apartment matches.")
                    } else {
                        LazyColumn(
                            Modifier.fillMaxSize(),
                            contentPadding = PaddingValues(start = 16.dp, end = 16.dp, top = 8.dp, bottom = 96.dp),
                            verticalArrangement = Arrangement.spacedBy(8.dp),
                        ) {
                            items(items.value, key = { it.id }) { a -> ApartmentRow(a) { onOpen(a.id) } }
                        }
                    }
                }
            }
        }
    }
}

@Composable
private fun ApartmentRow(a: Apartment, onClick: () -> Unit) {
    Card(onClick = onClick, modifier = Modifier.fillMaxWidth(), colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceContainer)) {
        Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
            Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.SpaceBetween, verticalAlignment = Alignment.CenterVertically) {
                Text(a.name, style = MaterialTheme.typography.titleMedium, fontWeight = FontWeight.ExtraBold, maxLines = 1, modifier = Modifier.weight(1f).padding(end = 8.dp))
                if (a.routers.isEmpty()) StatusChip("No router", Tone.Warn) else StatusChip("${a.onlineNow} online", if (a.onlineNow > 0) Tone.Good else Tone.Neutral)
            }
            if (a.address.isNotBlank()) Text(a.address, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurface, maxLines = 1)
            a.gps?.let { ViewOnMapChip(a.name, it) }
            Text(
                listOf(a.routers.joinToString(", ") { it.label }.ifBlank { "no router linked" }, "${a.accountsActive} of ${a.accountsTotal} houses active").joinToString("  |  "),
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
    }
}

// ---- detail -------------------------------------------------------------------------------------

@OptIn(ExperimentalLayoutApi::class)
@Composable
fun ApartmentDetailScreen(
    id: String,
    user: UserInfo,
    onBack: () -> Unit,
    onEdit: (String) -> Unit,
    onOpenAccount: (String) -> Unit,
    onOpenRouter: (String) -> Unit,
) {
    val container = LocalContainer.current
    val vm: ApartmentDetailViewModel = viewModel(
        key = "apartment-$id",
        factory = vmFactory { ApartmentDetailViewModel(id, container.apartments, container.sites) },
    )
    val ui by vm.state.collectAsStateWithLifecycle()
    val snack = remember { SnackbarHostState() }
    LaunchedEffect(ui.message) {
        ui.message?.let {
            snack.showSnackbar(it)
            vm.clearMessage()
        }
    }
    LaunchedEffect(ui.deleted) { if (ui.deleted) onBack() }

    Scaffold(
        containerColor = MaterialTheme.colorScheme.background,
        topBar = {
            AppTopBar(
                (ui.apartment as? Loadable.Ready)?.value?.name ?: "Apartment",
                onBack = onBack,
                actions = { if (user.canWrite) TextButton(onClick = { onEdit(id) }) { Text("Edit") } },
            )
        },
        snackbarHost = { SnackbarHost(snack) },
    ) { inner ->
        when (val a = ui.apartment) {
            Loadable.Loading -> CenteredLoading(Modifier.padding(inner))
            is Loadable.Failed -> ErrorState(a.message, vm::load, Modifier.padding(inner))
            is Loadable.Ready -> Column(
                Modifier.padding(inner).fillMaxSize().verticalScroll(rememberScrollState()).padding(16.dp),
                verticalArrangement = Arrangement.spacedBy(12.dp),
            ) {
                val apt = a.value
                SectionCard("Overview") {
                    if (apt.address.isNotBlank()) LabeledValue("Address", apt.address)
                    LabeledValue("Houses", "${apt.accountsActive} active of ${apt.accountsTotal}")
                    LabeledValue("Online now", apt.onlineNow.toString())
                    if (apt.notes.isNotBlank()) LabeledValue("Notes", apt.notes)
                    val point = apt.gps
                    if (point != null) {
                        LabeledValue("GPS", point.text)
                        ViewOnMapChip(apt.name, point)
                    } else {
                        LabeledValue("GPS", "Not recorded")
                    }
                }
                SectionCard("Routers") {
                    if (apt.routers.isEmpty()) {
                        Text(
                            "No router serves this apartment, so none of its houses can be set up on a router.",
                            style = MaterialTheme.typography.bodyMedium,
                            color = MaterialTheme.colorScheme.error,
                        )
                    }
                    apt.routers.forEach { r ->
                        Row(Modifier.fillMaxWidth(), verticalAlignment = Alignment.CenterVertically) {
                            TextButton(onClick = { onOpenRouter(r.key) }, modifier = Modifier.weight(1f)) {
                                Text("${r.label}  (${r.accounts} houses)", maxLines = 1)
                            }
                            if (user.canWrite) TextButton(onClick = { vm.askUnlink(r.key) }, enabled = !ui.busy) { Text("Unlink") }
                        }
                    }
                    if (user.canWrite && ui.linkable.isNotEmpty()) {
                        Text("Link a router", style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
                        FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                            ui.linkable.forEach { s ->
                                FilterChip(selected = false, onClick = { vm.link(s.key) }, enabled = !ui.busy, label = { ChipLabel(s.label) })
                            }
                        }
                    }
                }
                SectionCard("People") {
                    ContactBlock("Caretaker", apt.caretaker)
                    ContactBlock("Landlord / agent", apt.landlord)
                }
                SectionCard("Houses (${apt.accounts.size})") {
                    if (apt.accounts.isEmpty()) Text("No houses yet.", style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
                }
                apt.accounts.forEach { acc -> AccountRow(acc) { onOpenAccount(acc.id) } }
                if (user.isOwner) {
                    OutlinedButton(onClick = { vm.askDelete(true) }, enabled = !ui.busy, modifier = Modifier.fillMaxWidth()) { Text("Remove apartment") }
                }
            }
        }
    }

    ui.confirmUnlink?.let { rid ->
        val name = (ui.apartment as? Loadable.Ready)?.value?.routers?.firstOrNull { it.key == rid }?.label.orEmpty()
        AlertDialog(
            onDismissRequest = { vm.askUnlink(null) },
            title = { Text("Unlink $name?") },
            text = { Text("The router stops serving this apartment. It is refused while houses of this apartment are still on it: move them first.") },
            confirmButton = { TextButton(onClick = vm::unlink) { Text("Unlink") } },
            dismissButton = { TextButton(onClick = { vm.askUnlink(null) }) { Text("Cancel") } },
        )
    }
    if (ui.confirmDelete) {
        AlertDialog(
            onDismissRequest = { vm.askDelete(false) },
            title = { Text("Remove this apartment?") },
            text = { Text("Only possible when no customer, open or closed, belongs to it.") },
            confirmButton = { TextButton(onClick = vm::delete) { Text("Remove") } },
            dismissButton = { TextButton(onClick = { vm.askDelete(false) }) { Text("Cancel") } },
        )
    }
}

/** "View on map": opens the building's point in the phone's maps app. */
@Composable
fun ViewOnMapChip(name: String, point: GpsPoint, modifier: Modifier = Modifier) {
    val context = LocalContext.current
    AssistChip(
        onClick = { openMap(context, name, point) },
        label = { ChipLabel("View on map") },
        leadingIcon = { Icon(Icons.Default.Place, contentDescription = null) },
        modifier = modifier,
    )
}

/**
 * Opens a maps app on the point with a labelled pin (`geo:` URI). When no app handles `geo:`, falls back to the
 * Google Maps web link, which the browser opens. A phone with neither must not crash the screen. Returns whether
 * something opened.
 */
internal fun openMap(context: Context, name: String, point: GpsPoint): Boolean {
    fun view(uri: String): Boolean {
        val intent = Intent(Intent.ACTION_VIEW, Uri.parse(uri))
        if (context !is Activity) intent.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
        return try {
            context.startActivity(intent)
            true
        } catch (_: ActivityNotFoundException) {
            false
        } catch (_: SecurityException) {
            false
        }
    }
    return view(point.geoUri(name)) || view(point.webUrl)
}

/** Name, phones with tap-to-call and WhatsApp, email and notes of a caretaker or landlord. */
@OptIn(ExperimentalLayoutApi::class)
@Composable
fun ContactBlock(title: String, c: ApartmentContact) {
    val uri = LocalUriHandler.current
    // A phone with no dialer or WhatsApp installed must not crash the screen.
    fun open(target: String) {
        runCatching { uri.openUri(target) }
    }
    Column(verticalArrangement = Arrangement.spacedBy(4.dp)) {
        Text(title, style = MaterialTheme.typography.labelLarge, color = MaterialTheme.colorScheme.onSurfaceVariant)
        if (c.isEmpty) {
            Text("Not recorded", style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
            return@Column
        }
        if (c.name.isNotBlank()) Text(c.name, style = MaterialTheme.typography.titleMedium, color = MaterialTheme.colorScheme.onSurface)
        FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            c.telUri?.let { FilledTonalButton(onClick = { open(it) }) { Text("Call ${c.phone}", maxLines = 1) } }
            if (c.whatsappUrl.isNotBlank()) OutlinedButton(onClick = { open(c.whatsappUrl) }) { Text("WhatsApp", maxLines = 1) }
            if (c.phone2.isNotBlank()) OutlinedButton(onClick = { open("tel:${c.phone2}") }) { Text("Call ${c.phone2}", maxLines = 1) }
        }
        if (c.email.isNotBlank()) Text(c.email, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurface)
        if (c.notes.isNotBlank()) Text(c.notes, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
    }
}

// ---- create / edit ------------------------------------------------------------------------------

@Composable
fun ApartmentFormScreen(id: String?, onBack: () -> Unit, onSaved: (String) -> Unit) {
    val container = LocalContainer.current
    val vm: ApartmentFormViewModel = viewModel(
        key = "apartment-form-${id.orEmpty()}",
        factory = vmFactory { ApartmentFormViewModel(id, container.apartments, container.sites) },
    )
    val ui by vm.state.collectAsStateWithLifecycle()
    LaunchedEffect(ui.savedId) { ui.savedId?.let(onSaved) }

    Scaffold(
        containerColor = MaterialTheme.colorScheme.background,
        topBar = { AppTopBar(if (ui.isNew) "New apartment" else "Edit apartment", onBack = onBack) },
        bottomBar = {
            Button(
                onClick = vm::save,
                enabled = ui.canSave,
                modifier = Modifier.fillMaxWidth().padding(16.dp),
            ) { Text(if (ui.saving) "Saving..." else "Save", fontWeight = FontWeight.ExtraBold) }
        },
    ) { inner ->
        if (ui.loading) {
            CenteredLoading(Modifier.padding(inner))
        } else {
            Column(
                Modifier.padding(inner).fillMaxSize().verticalScroll(rememberScrollState()).padding(16.dp),
                verticalArrangement = Arrangement.spacedBy(12.dp),
            ) {
                OutlinedTextField(
                    ui.form.name, vm::onName, label = { Text("Apartment name") }, singleLine = true,
                    isError = ui.fieldErrors.containsKey("name"), supportingText = { ui.fieldErrors["name"]?.let { Text(it) } },
                    modifier = Modifier.fillMaxWidth(),
                )
                OutlinedTextField(ui.form.address, vm::onAddress, label = { Text("Address or area") }, singleLine = true, modifier = Modifier.fillMaxWidth())
                OutlinedTextField(ui.form.notes, vm::onNotes, label = { Text("Notes") }, modifier = Modifier.fillMaxWidth())
                OutlinedTextField(
                    ui.gpsText, vm::onGps, label = { Text("GPS location (optional)") }, singleLine = true,
                    placeholder = { Text("-1.2921, 36.8219") },
                    isError = ui.fieldErrors.containsKey("gps") || ui.fieldErrors.containsKey("lat") || ui.fieldErrors.containsKey("lng"),
                    supportingText = {
                        Text(
                            ui.fieldErrors["gps"] ?: ui.fieldErrors["lat"] ?: ui.fieldErrors["lng"]
                                ?: "Paste the coordinates or a Google Maps link from the map app. Leave empty if unknown.",
                        )
                    },
                    modifier = Modifier.fillMaxWidth(),
                )
                if (ui.isNew) {
                    SectionCard("Routers that serve it") {
                        if (ui.allRouters.isEmpty()) Text("No routers yet.", color = MaterialTheme.colorScheme.onSurfaceVariant)
                        ui.allRouters.forEach { r ->
                            Row(verticalAlignment = Alignment.CenterVertically) {
                                Checkbox(checked = r.key in ui.routerIds, onCheckedChange = { vm.toggleRouter(r.key) })
                                Text(r.label, style = MaterialTheme.typography.bodyLarge)
                            }
                        }
                    }
                }
                ContactFields("Caretaker (optional)", "caretaker", ui.form.caretaker, ui.fieldErrors, vm::onCaretaker)
                ContactFields("Landlord / agent (optional)", "landlord", ui.form.landlord, ui.fieldErrors, vm::onLandlord)
                ui.error?.let { Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodyMedium) }
            }
        }
    }
}

@Composable
private fun ContactFields(
    title: String,
    key: String,
    c: ApartmentContact,
    errors: Map<String, String>,
    onChange: (ApartmentContact) -> Unit,
) {
    SectionCard(title) {
        OutlinedTextField(c.name, { onChange(c.copy(name = it)) }, label = { Text("Name") }, singleLine = true, modifier = Modifier.fillMaxWidth())
        OutlinedTextField(
            c.phone, { onChange(c.copy(phone = it)) }, label = { Text("Phone") }, singleLine = true,
            keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Phone),
            isError = errors.containsKey("$key.phone"), supportingText = { Text(errors["$key.phone"] ?: "07.. or +254..; saved as +254...") },
            modifier = Modifier.fillMaxWidth(),
        )
        OutlinedTextField(
            c.phone2, { onChange(c.copy(phone2 = it)) }, label = { Text("Alternative phone") }, singleLine = true,
            keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Phone),
            isError = errors.containsKey("$key.phone2"), supportingText = { errors["$key.phone2"]?.let { Text(it) } },
            modifier = Modifier.fillMaxWidth(),
        )
        OutlinedTextField(
            c.email, { onChange(c.copy(email = it)) }, label = { Text("Email (optional)") }, singleLine = true,
            keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Email),
            isError = errors.containsKey("$key.email"), supportingText = { errors["$key.email"]?.let { Text(it) } },
            modifier = Modifier.fillMaxWidth(),
        )
        OutlinedTextField(c.notes, { onChange(c.copy(notes = it)) }, label = { Text("Notes") }, modifier = Modifier.fillMaxWidth())
    }
}
