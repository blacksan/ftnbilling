package ke.co.ftw.ftnadmin.ui

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.cancel
import kotlinx.coroutines.test.StandardTestDispatcher
import kotlinx.coroutines.test.TestDispatcher
import kotlinx.coroutines.test.resetMain
import kotlinx.coroutines.test.setMain
import org.junit.rules.TestWatcher
import org.junit.runner.Description

/**
 * Replaces Dispatchers.Main so viewModelScope works in JVM tests.
 *
 * View models made with [track] have their scope cancelled in [cancelViewModels] (call it first in `@After`,
 * before the server stops). Without that, a call still in flight when the test ends is resumed on a Main that
 * has been reset, and the failure is reported against whichever test runs next (UncaughtExceptionsBeforeTest).
 * [finished] cancels them too, as a safety net, before Main is reset.
 *
 * Pass [dispatcher] as the `io` of `ApiClient.create` so response bodies are read without hopping to a real thread.
 */
@OptIn(ExperimentalCoroutinesApi::class)
class MainDispatcherRule(val dispatcher: TestDispatcher = StandardTestDispatcher()) : TestWatcher() {
    private val tracked = mutableListOf<ViewModel>()

    /** Remembers [vm] so its scope is cancelled when the test ends. */
    fun <T : ViewModel> track(vm: T): T {
        synchronized(tracked) { tracked += vm }
        return vm
    }

    /** Cancels every tracked view model and lets the cancellations run. */
    fun cancelViewModels() {
        val vms = synchronized(tracked) { tracked.toList().also { tracked.clear() } }
        vms.forEach { it.viewModelScope.cancel() }
        dispatcher.scheduler.advanceUntilIdle()
    }

    override fun starting(description: Description) = Dispatchers.setMain(dispatcher)

    override fun finished(description: Description) {
        cancelViewModels()
        Dispatchers.resetMain()
    }
}
