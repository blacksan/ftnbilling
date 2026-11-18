package ke.co.ftw.ftnadmin.ui.admin

import ke.co.ftw.ftnadmin.ui.common.LoadingSpinner
import android.net.Uri
import androidx.activity.compose.BackHandler
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.padding
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Home
import androidx.compose.material.icons.filled.MoreVert
import androidx.compose.material.icons.filled.Refresh
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.Checkbox
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.key
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberUpdatedState
import androidx.compose.runtime.setValue
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.dp
import androidx.compose.ui.window.DialogProperties
import androidx.compose.ui.window.SecureFlagPolicy
import androidx.compose.ui.viewinterop.AndroidView
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import ke.co.ftw.ftnadmin.BuildConfig
import ke.co.ftw.ftnadmin.tunnel.TunnelState
import ke.co.ftw.ftnadmin.ui.common.LocalContainer
import ke.co.ftw.ftnadmin.ui.common.isDeviceSecure
import ke.co.ftw.ftnadmin.ui.common.openInBrowser
import ke.co.ftw.ftnadmin.ui.common.rememberConnectAction
import ke.co.ftw.ftnadmin.web.AdminWebController
import ke.co.ftw.ftnadmin.web.AuthGate
import ke.co.ftw.ftnadmin.web.ExportDownloader
import ke.co.ftw.ftnadmin.web.UrlPolicy
import ke.co.ftw.ftnadmin.web.WebOverlay

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun AdminScreen(path: String?, onHome: () -> Unit) {
    val container = LocalContainer.current
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    val currentOnHome by rememberUpdatedState(onHome)
    val policy = remember { UrlPolicy(BuildConfig.ADMIN_HOST, BuildConfig.ADMIN_PORT) }
    val controller = remember {
        val gate = AuthGate(
            adminHost = BuildConfig.ADMIN_HOST,
            isTunnelUp = { container.tunnel.state.value is TunnelState.Up },
            storedCredentials = { container.credentials.current() },
        )
        AdminWebController(
            context = context,
            scope = scope,
            policy = policy,
            gate = gate,
            credentials = container.credentials,
            downloader = ExportDownloader(context, container.adminApi, policy),
            onLogoutRequested = {
                container.logout()
                currentOnHome()
            },
            onExternal = { context.openInBrowser(it) },
        )
    }
    // The nav argument is already decoded; tolerate a still-encoded value rather than loading the wrong page.
    val startUrl = remember(path) {
        val decoded = if (path?.startsWith("%2F") == true) Uri.decode(path) else path
        policy.adminUrl(decoded) ?: policy.adminUrl(null)
    }

    val tunnelState by container.tunnel.state.collectAsStateWithLifecycle()
    val overlay by controller.overlay.collectAsStateWithLifecycle()
    val progress by controller.progress.collectAsStateWithLifecycle()
    val canGoBack by controller.canGoBack.collectAsStateWithLifecycle()
    val jsDialog by controller.jsDialog.collectAsStateWithLifecycle()
    val pendingExport by controller.pendingExport.collectAsStateWithLifecycle()
    val notice by controller.notice.collectAsStateWithLifecycle()
    val generation by controller.generation.collectAsStateWithLifecycle()
    val connect = rememberConnectAction()
    val snackbar = remember { SnackbarHostState() }
    var menuOpen by remember { mutableStateOf(false) }
    val tunnelUp = tunnelState is TunnelState.Up

    val exportPicker = rememberLauncherForActivityResult(ActivityResultContracts.CreateDocument("text/csv")) {
        controller.onExportLocationChosen(it)
    }
    LaunchedEffect(pendingExport) { pendingExport?.let { exportPicker.launch(it.fileName) } }
    LaunchedEffect(notice) {
        notice?.let {
            snackbar.showSnackbar(it)
            controller.consumeNotice()
        }
    }
    // Opening the admin implies the intent to connect.
    LaunchedEffect(Unit) {
        if (container.tunnel.state.value is TunnelState.Down && container.tunnel.prepareIntent() == null) {
            container.tunnel.connect()
        }
    }
    BackHandler(enabled = tunnelUp && canGoBack) { controller.goBack() }

    Scaffold(
        topBar = {
            TopAppBar(
                title = { Text("Admin") },
                navigationIcon = {
                    IconButton(onClick = onHome) { Icon(Icons.Default.Home, contentDescription = "Home") }
                },
                actions = {
                    IconButton(onClick = controller::reload, enabled = tunnelUp) {
                        Icon(Icons.Default.Refresh, contentDescription = "Reload")
                    }
                    Box {
                        IconButton(onClick = { menuOpen = true }) { Icon(Icons.Default.MoreVert, contentDescription = "More") }
                        DropdownMenu(expanded = menuOpen, onDismissRequest = { menuOpen = false }) {
                            DropdownMenuItem(
                                text = { Text("Print") },
                                enabled = tunnelUp,
                                onClick = {
                                    menuOpen = false
                                    controller.print(context)
                                },
                            )
                        }
                    }
                },
            )
        },
        snackbarHost = { SnackbarHost(snackbar) },
    ) { padding ->
        Box(Modifier.fillMaxSize().padding(padding).imePadding()) {
            if (tunnelUp) {
                key(generation) {
                    AndroidView(
                        factory = { ctx -> controller.createWebView(ctx, startUrl.orEmpty()) },
                        onRelease = { controller.destroyWebView(it) },
                        modifier = Modifier.fillMaxSize(),
                    )
                }
                if (progress in 1..99) {
                    LinearProgressIndicator(progress = { progress / 100f }, modifier = Modifier.fillMaxWidth())
                }
                OverlayPanel(overlay, onRetry = controller::reload, onHome = onHome)
            } else {
                TunnelPanel(tunnelState, onConnect = connect)
            }
        }
    }

    if (tunnelUp && overlay is WebOverlay.SignIn) {
        SignInDialog(
            initialUser = controller.defaultUsername(),
            canRemember = context.isDeviceSecure(),
            onSubmit = controller::submitCredentials,
            onCancel = onHome,
        )
    }
    jsDialog?.let { dialog ->
        val dismiss: (@Composable () -> Unit)? = if (dialog.isConfirm) {
            { TextButton(onClick = { controller.dismissJsDialog(false) }) { Text("Cancel") } }
        } else {
            null
        }
        AlertDialog(
            onDismissRequest = { controller.dismissJsDialog(false) },
            text = { Text(dialog.message) },
            confirmButton = { TextButton(onClick = { controller.dismissJsDialog(true) }) { Text("OK") } },
            dismissButton = dismiss,
        )
    }
}

@Composable
private fun TunnelPanel(state: TunnelState, onConnect: () -> Unit) {
    Column(
        Modifier.fillMaxSize().padding(32.dp),
        verticalArrangement = Arrangement.Center,
        horizontalAlignment = Alignment.CenterHorizontally,
    ) {
        when (state) {
            is TunnelState.Connecting -> {
                Text("Connecting", style = MaterialTheme.typography.titleLarge)
                Text(state.stage, textAlign = TextAlign.Center)
                Spacer(Modifier.height(16.dp))
                LoadingSpinner()
            }
            is TunnelState.Error -> {
                Text("Could not connect", style = MaterialTheme.typography.titleLarge)
                Text(state.message, textAlign = TextAlign.Center, color = MaterialTheme.colorScheme.error)
                Spacer(Modifier.height(16.dp))
                Button(onClick = onConnect) { Text("Try again") }
            }
            TunnelState.NeedsPermission -> {
                Text("VPN permission needed", style = MaterialTheme.typography.titleLarge)
                Text("Allow the VPN request to reach the admin server.", textAlign = TextAlign.Center)
                Spacer(Modifier.height(16.dp))
                Button(onClick = onConnect) { Text("Allow and connect") }
            }
            TunnelState.NoConfig -> Text("No tunnel is set up.", style = MaterialTheme.typography.titleLarge)
            else -> {
                Text("Tunnel is down", style = MaterialTheme.typography.titleLarge)
                Text("The admin pages are only reachable through the tunnel.", textAlign = TextAlign.Center)
                Spacer(Modifier.height(16.dp))
                Button(onClick = onConnect) { Text("Connect") }
            }
        }
    }
}

@Composable
private fun OverlayPanel(overlay: WebOverlay, onRetry: () -> Unit, onHome: () -> Unit) {
    val (title, body) = when (overlay) {
        WebOverlay.None, WebOverlay.SignIn -> return
        is WebOverlay.LockedOut -> "Too many sign-in attempts" to
            (overlay.retryAfterSec?.let { "The server is rate limiting this device. Try again in about ${(it + 59) / 60} min." }
                ?: "The server is rate limiting this device. Try again in a few minutes.")
        WebOverlay.NotOnAdminNetwork -> "This device is not on the admin network" to
            "The server hid the admin pages from this address. Check that the tunnel is connected and this device is still registered."
        is WebOverlay.LoadError -> "Page could not be loaded" to overlay.message
    }
    Surface(Modifier.fillMaxSize()) {
        Column(
            Modifier.padding(32.dp),
            verticalArrangement = Arrangement.Center,
            horizontalAlignment = Alignment.CenterHorizontally,
        ) {
            Text(title, style = MaterialTheme.typography.titleLarge, textAlign = TextAlign.Center)
            Spacer(Modifier.height(8.dp))
            Text(body, textAlign = TextAlign.Center)
            Spacer(Modifier.height(16.dp))
            Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                Button(onClick = onRetry) { Text("Retry") }
                OutlinedButton(onClick = onHome) { Text("Home") }
            }
        }
    }
}

@Composable
private fun SignInDialog(
    initialUser: String,
    canRemember: Boolean,
    onSubmit: (user: String, password: String, remember: Boolean) -> Unit,
    onCancel: () -> Unit,
) {
    var user by remember { mutableStateOf(initialUser) }
    var pass by remember { mutableStateOf("") }
    var save by remember { mutableStateOf(canRemember) }
    AlertDialog(
        onDismissRequest = onCancel,
        // A stray tap outside must not throw the owner back to Home mid-typing.
        properties = DialogProperties(dismissOnClickOutside = false, securePolicy = SecureFlagPolicy.SecureOn),
        title = { Text("Sign in to admin") },
        text = {
            Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                OutlinedTextField(user, { user = it }, label = { Text("Username") }, singleLine = true)
                OutlinedTextField(
                    pass,
                    { pass = it },
                    label = { Text("Password") },
                    singleLine = true,
                    visualTransformation = PasswordVisualTransformation(),
                    keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Password),
                )
                if (canRemember) {
                    Row(verticalAlignment = Alignment.CenterVertically) {
                        Checkbox(save, { save = it })
                        Spacer(Modifier.width(4.dp))
                        Text("Remember on this device")
                    }
                }
            }
        },
        confirmButton = {
            TextButton(onClick = { onSubmit(user, pass, save && canRemember) }, enabled = user.isNotBlank() && pass.isNotEmpty()) {
                Text("Sign in")
            }
        },
        dismissButton = { TextButton(onClick = onCancel) { Text("Cancel") } },
    )
}
