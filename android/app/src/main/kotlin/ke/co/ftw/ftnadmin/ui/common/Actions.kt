package ke.co.ftw.ftnadmin.ui.common

import android.util.Log
import ke.co.ftw.ftnadmin.AppContainer
import ke.co.ftw.ftnadmin.auth.Credentials
import ke.co.ftw.ftnadmin.tunnel.InvalidTunnelConfigException
import ke.co.ftw.ftnadmin.tunnel.TunnelState
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext

private const val TAG = "Actions"

/** Validates and stores a tunnel config. Returns an error message for the owner, or null on success. */
suspend fun AppContainer.importTunnel(text: String): String? {
    val state = tunnel.state.value
    return try {
        // Parse first so a bad paste never drops a working connection.
        withContext(Dispatchers.IO) { tunnelRepository.import(text) }
        if (state is TunnelState.Up || state is TunnelState.Connecting) tunnel.disconnect()
        null
    } catch (e: InvalidTunnelConfigException) {
        e.message
    } catch (e: Exception) {
        Log.w(TAG, "import failed", e)
        "The tunnel could not be stored securely on this device."
    }
}

/** Encrypts and stores the admin login. Returns an error message, or null on success. */
suspend fun AppContainer.saveLogin(username: String, password: String): String? {
    if (username.isBlank() || password.isEmpty()) return "Enter the admin username and password."
    return try {
        withContext(Dispatchers.IO) { credentials.save(Credentials(username.trim(), password)) }
        null
    } catch (e: Exception) {
        Log.w(TAG, "saving login failed", e)
        "The login could not be stored securely on this device."
    }
}
