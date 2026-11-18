package ke.co.ftw.ftnadmin.ui.common

import androidx.lifecycle.ViewModel
import androidx.lifecycle.ViewModelProvider
import androidx.lifecycle.viewmodel.initializer
import androidx.lifecycle.viewmodel.viewModelFactory
import ke.co.ftw.ftnadmin.core.network.ApiError
import ke.co.ftw.ftnadmin.core.network.ErrorCode
import kotlinx.coroutines.CancellationException

/** A load that is in flight, finished, or failed. Screens render this and nothing else. */
sealed interface Loadable<out T> {
    data object Loading : Loadable<Nothing>

    data class Failed(val message: String) : Loadable<Nothing>

    data class Ready<T>(val value: T) : Loadable<T>
}

/** Runs [block] and converts any failure to an [ApiError]; cancellation still propagates. */
suspend fun <T> apiResult(block: suspend () -> T): Result<T> = try {
    Result.success(block())
} catch (e: CancellationException) {
    throw e
} catch (e: ApiError) {
    Result.failure(e)
} catch (e: Exception) {
    Result.failure(ApiError(0, ErrorCode.UNKNOWN, "Something went wrong: ${e.message ?: e::class.simpleName}", cause = e))
}

/** Plain-words message for the screen. */
fun Throwable.userMessage(): String {
    val e = this as? ApiError ?: return message ?: "Something went wrong."
    return when (e.code) {
        ErrorCode.RATE_LIMITED -> e.retryAfterS?.let { "Too many attempts. Try again in $it s." } ?: "Too many attempts. Try again shortly."
        ErrorCode.FORBIDDEN_ROLE -> "Your account is not allowed to do this."
        ErrorCode.DB_UNAVAILABLE -> "The server database is unavailable. Try again in a moment."
        ErrorCode.TOKEN_EXPIRED, ErrorCode.TOKEN_REVOKED -> "Your session ended. Sign in again."
        else -> e.fieldErrors.entries.firstOrNull()?.let { "${it.key}: ${it.value}" } ?: e.message
    }
}

/** ViewModel factory for the manual DI graph: `vmFactory { MyViewModel(container.repo) }`. */
inline fun <reified VM : ViewModel> vmFactory(crossinline create: () -> VM): ViewModelProvider.Factory =
    viewModelFactory { initializer { create() } }
