package ke.co.ftw.ftnadmin.ui.login

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.safeDrawingPadding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.KeyboardActions
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Button
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import ke.co.ftw.ftnadmin.core.network.AuthState
import ke.co.ftw.ftnadmin.ui.common.LocalContainer
import ke.co.ftw.ftnadmin.ui.common.SecureScreen
import ke.co.ftw.ftnadmin.ui.common.vmFactory

@Composable
fun LoginScreen() {
    SecureScreen()
    val container = LocalContainer.current
    val notice = (container.sessions.state.value as? AuthState.SignedOut)?.reason
    val vm: LoginViewModel = viewModel(factory = vmFactory { LoginViewModel(container.auth, notice) })
    val state by vm.state.collectAsStateWithLifecycle()
    LoginContent(
        state = state,
        onUsername = vm::onUsername,
        onPassword = vm::onPassword,
        onTotp = vm::onTotp,
        onRecovery = vm::onRecovery,
        onSetupCode = vm::onSetupCode,
        onToggleMode = vm::toggleMode,
        onToggleSetup = vm::toggleSetupCode,
        onSubmit = vm::submit,
    )
}

@Composable
fun LoginContent(
    state: LoginUiState,
    onUsername: (String) -> Unit,
    onPassword: (String) -> Unit,
    onTotp: (String) -> Unit,
    onRecovery: (String) -> Unit,
    onSetupCode: (String) -> Unit,
    onToggleMode: () -> Unit,
    onToggleSetup: () -> Unit,
    onSubmit: () -> Unit,
) {
    Scaffold(containerColor = MaterialTheme.colorScheme.background) { inner ->
        Column(
            Modifier.fillMaxSize().padding(inner).safeDrawingPadding().imePadding()
                .verticalScroll(rememberScrollState()).padding(24.dp),
            verticalArrangement = Arrangement.spacedBy(12.dp, Alignment.CenterVertically),
        ) {
            Text("Fast Tech Net", style = MaterialTheme.typography.headlineMedium, color = MaterialTheme.colorScheme.primary)
            Text("Sign in to the operator console", style = MaterialTheme.typography.bodyLarge, color = MaterialTheme.colorScheme.onSurfaceVariant)
            Spacer(Modifier.height(8.dp))

            OutlinedTextField(
                value = state.username,
                onValueChange = onUsername,
                label = { Text("Username") },
                singleLine = true,
                enabled = !state.busy,
                keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Text, imeAction = ImeAction.Next),
                modifier = Modifier.fillMaxWidth(),
            )
            OutlinedTextField(
                value = state.password,
                onValueChange = onPassword,
                label = { Text("Password") },
                singleLine = true,
                enabled = !state.busy,
                visualTransformation = PasswordVisualTransformation(),
                keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Password, imeAction = ImeAction.Next),
                modifier = Modifier.fillMaxWidth(),
            )
            if (state.mode == FactorMode.Totp) {
                OutlinedTextField(
                    value = state.totp,
                    onValueChange = onTotp,
                    label = { Text("Authenticator code") },
                    supportingText = { Text("6 digits from your authenticator app") },
                    singleLine = true,
                    enabled = !state.busy,
                    keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.NumberPassword, imeAction = ImeAction.Done),
                    keyboardActions = KeyboardActions(onDone = { onSubmit() }),
                    modifier = Modifier.fillMaxWidth(),
                )
            } else {
                OutlinedTextField(
                    value = state.recoveryCode,
                    onValueChange = onRecovery,
                    label = { Text("Recovery code") },
                    supportingText = { Text("One of your saved single-use codes") },
                    singleLine = true,
                    enabled = !state.busy,
                    keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Ascii, imeAction = ImeAction.Done),
                    keyboardActions = KeyboardActions(onDone = { onSubmit() }),
                    modifier = Modifier.fillMaxWidth(),
                )
            }
            TextButton(onClick = onToggleMode, enabled = !state.busy) {
                Text(if (state.mode == FactorMode.Totp) "Use a recovery code instead" else "Use the authenticator code instead")
            }
            if (state.showSetupCode) {
                OutlinedTextField(
                    value = state.setupCode,
                    onValueChange = onSetupCode,
                    label = { Text("Setup code (first sign-in only)") },
                    singleLine = true,
                    enabled = !state.busy,
                    modifier = Modifier.fillMaxWidth(),
                )
            } else {
                TextButton(onClick = onToggleSetup, enabled = !state.busy) { Text("I have a setup code") }
            }

            state.error?.let {
                Text(it, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.error)
            }

            Button(onClick = onSubmit, enabled = state.canSubmit, modifier = Modifier.fillMaxWidth()) {
                Text(if (state.busy) "Signing in..." else "Sign in")
            }
        }
    }
}
