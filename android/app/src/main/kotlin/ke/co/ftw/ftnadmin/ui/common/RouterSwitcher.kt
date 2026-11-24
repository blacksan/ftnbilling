package ke.co.ftw.ftnadmin.ui.common

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.WindowInsets
import androidx.compose.foundation.layout.WindowInsetsSides
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.only
import androidx.compose.foundation.layout.safeDrawing
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.layout.windowInsetsPadding
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.ArrowDropDown
import androidx.compose.material.icons.filled.Check
import androidx.compose.material.icons.filled.Notifications
import androidx.compose.material3.Badge
import androidx.compose.material3.BadgedBox
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.ScaffoldDefaults
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TopAppBar
import androidx.compose.material3.TopAppBarDefaults
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.res.pluralStringResource
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import ke.co.ftw.ftnadmin.R
import ke.co.ftw.ftnadmin.domain.Site
import ke.co.ftw.ftnadmin.ui.theme.AppTheme

/** Status dot colour: green online, red offline. */
@Composable
fun statusDotColor(online: Boolean?): Color = when (online) {
    true -> AppTheme.extended.success
    false -> MaterialTheme.colorScheme.error
    null -> MaterialTheme.colorScheme.outline
}

/** The dot says its state to TalkBack; where it sits beside the same word, pass [describe] = false. */
@Composable
fun StatusDot(online: Boolean?, modifier: Modifier = Modifier, size: androidx.compose.ui.unit.Dp = 10.dp, describe: Boolean = true) {
    val words = stringResource(onlineStateRes(online))
    val m = if (describe) modifier.semantics { contentDescription = words } else modifier
    Surface(m.size(size), shape = CircleShape, color = statusDotColor(online)) {}
}

/** The word for an online state: Online, Offline, or Unknown when not known. */
fun onlineStateRes(online: Boolean?): Int = when (online) {
    true -> R.string.state_online
    false -> R.string.state_offline
    null -> R.string.state_unknown
}

/**
 * Top-bar router switcher: "All routers" plus every router with a status dot. The choice is saved in
 * settings through [ke.co.ftw.ftnadmin.data.routers.RouterContext].
 */
@Composable
fun RouterSwitcher(modifier: Modifier = Modifier) {
    val ctx = LocalContainer.current.routerContext
    val routers by ctx.routers.collectAsStateWithLifecycle()
    val selection by ctx.selection.collectAsStateWithLifecycle()
    var open by remember { mutableStateOf(false) }
    val list: List<Site> = (routers as? Loadable.Ready)?.value.orEmpty()
    val current = list.firstOrNull { it.key == selection }
    val allOnline: Boolean? = if (list.isEmpty()) null else list.all { it.online }
    val label = current?.label ?: stringResource(R.string.all_routers)
    val stateWord = stringResource(onlineStateRes(current?.online ?: allOnline))
    val switchDesc = stringResource(R.string.router_switcher_desc, label, stateWord)

    Box(modifier) {
        Surface(
            onClick = { open = true },
            shape = MaterialTheme.shapes.extraLarge,
            color = MaterialTheme.colorScheme.secondaryContainer,
            contentColor = MaterialTheme.colorScheme.onSecondaryContainer,
            modifier = Modifier.semantics { contentDescription = switchDesc },
        ) {
            Row(
                Modifier.padding(start = 14.dp, end = 6.dp, top = 6.dp, bottom = 6.dp).widthIn(max = 260.dp),
                verticalAlignment = Alignment.CenterVertically,
                horizontalArrangement = Arrangement.spacedBy(8.dp),
            ) {
                StatusDot(current?.online ?: allOnline, describe = false)
                Text(label, style = MaterialTheme.typography.labelLarge, fontWeight = FontWeight.Bold, maxLines = 1, overflow = TextOverflow.Ellipsis, modifier = Modifier.weight(1f, fill = false))
                Icon(Icons.Default.ArrowDropDown, contentDescription = null)
            }
        }
        DropdownMenu(expanded = open, onDismissRequest = { open = false }) {
            DropdownMenuItem(
                text = { Text(stringResource(R.string.all_routers), fontWeight = FontWeight.Bold) },
                leadingIcon = { StatusDot(allOnline) },
                trailingIcon = { if (selection == null) Icon(Icons.Default.Check, contentDescription = stringResource(R.string.selected)) },
                onClick = {
                    ctx.select(null)
                    open = false
                },
            )
            list.forEach { r ->
                DropdownMenuItem(
                    text = { Text(r.label, maxLines = 1, overflow = TextOverflow.Ellipsis) },
                    leadingIcon = { StatusDot(r.online) },
                    trailingIcon = { if (r.key == selection) Icon(Icons.Default.Check, contentDescription = stringResource(R.string.selected)) },
                    onClick = {
                        ctx.select(r.key)
                        open = false
                    },
                )
            }
        }
    }
}

/** Bell with the open-alert count of the selected router (or all routers). */
@Composable
private fun AlertsBell(onClick: () -> Unit) {
    val ctx = LocalContainer.current.routerContext
    val routers by ctx.routers.collectAsStateWithLifecycle()
    val selection by ctx.selection.collectAsStateWithLifecycle()
    val count = (routers as? Loadable.Ready)?.value.orEmpty().filter { selection == null || it.key == selection }.sumOf { it.openAlerts }
    IconButton(onClick = onClick) {
        BadgedBox(badge = { if (count > 0) Badge { Text(if (count > 99) "99+" else count.toString()) } }) {
            Icon(Icons.Default.Notifications, contentDescription = if (count > 0) pluralStringResource(R.plurals.alerts_open_desc, count, count) else stringResource(R.string.alerts))
        }
    }
}

/** Top bar of the five tabs: bold title, alerts bell, and the router switcher on its own row. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun TabTopBar(title: String, onAlerts: () -> Unit) {
    Column(Modifier.fillMaxWidth()) {
        TopAppBar(
            title = { Text(title, maxLines = 1, overflow = TextOverflow.Ellipsis, fontWeight = FontWeight.ExtraBold) },
            actions = { AlertsBell(onAlerts) },
            colors = TopAppBarDefaults.topAppBarColors(containerColor = MaterialTheme.colorScheme.background),
        )
        Row(
            Modifier.fillMaxWidth().windowInsetsPadding(WindowInsets.safeDrawing.only(WindowInsetsSides.Horizontal)).padding(start = 16.dp, end = 16.dp, bottom = 8.dp),
        ) { RouterSwitcher() }
    }
}

/**
 * Scaffold for the five tabs. The navigation bar sits outside it, so only the side insets are taken
 * here; the top bar handles the status bar itself.
 */
@Composable
fun TabScaffold(
    title: String,
    onAlerts: () -> Unit,
    modifier: Modifier = Modifier,
    snackbarHost: @Composable () -> Unit = {},
    floatingActionButton: @Composable () -> Unit = {},
    content: @Composable (androidx.compose.foundation.layout.PaddingValues) -> Unit,
) {
    Scaffold(
        modifier = modifier,
        containerColor = MaterialTheme.colorScheme.background,
        contentWindowInsets = ScaffoldDefaults.contentWindowInsets.only(WindowInsetsSides.Horizontal),
        topBar = { TabTopBar(title, onAlerts) },
        snackbarHost = snackbarHost,
        floatingActionButton = floatingActionButton,
        content = content,
    )
}

