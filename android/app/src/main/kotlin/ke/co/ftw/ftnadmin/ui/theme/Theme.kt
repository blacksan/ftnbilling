package ke.co.ftw.ftnadmin.ui.theme

import android.os.Build
import androidx.annotation.ChecksSdkIntAtLeast
import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.material3.ExperimentalMaterial3ExpressiveApi
import androidx.compose.material3.MaterialExpressiveTheme
import androidx.compose.material3.MotionScheme
import androidx.compose.material3.dynamicDarkColorScheme
import androidx.compose.material3.dynamicLightColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.runtime.ReadOnlyComposable
import androidx.compose.runtime.staticCompositionLocalOf
import androidx.compose.ui.platform.LocalContext

val LocalExtendedColors = staticCompositionLocalOf { FastTechNetBrand.extended }

/** Material 3 Expressive theme. Dynamic colour is opt-in so the brand stays the default. */
@OptIn(ExperimentalMaterial3ExpressiveApi::class)
@Composable
fun AppTheme(
    brand: Brand = FastTechNetBrand,
    darkTheme: Boolean = isSystemInDarkTheme(),
    dynamicColor: Boolean = false,
    content: @Composable () -> Unit,
) {
    val colorScheme = if (dynamicColor && supportsDynamicTheming()) {
        val context = LocalContext.current
        if (darkTheme) dynamicDarkColorScheme(context) else dynamicLightColorScheme(context)
    } else {
        if (darkTheme) brand.dark else brand.light
    }
    CompositionLocalProvider(
        LocalExtendedColors provides if (darkTheme) brand.extendedDark else brand.extended,
    ) {
        MaterialExpressiveTheme(
            colorScheme = colorScheme,
            motionScheme = MotionScheme.expressive(),
            shapes = AppShapes,
            typography = AppTypography,
            content = content,
        )
    }
}

object AppTheme {
    val extended: ExtendedColors
        @Composable
        @ReadOnlyComposable
        get() = LocalExtendedColors.current
}

@ChecksSdkIntAtLeast(api = Build.VERSION_CODES.S)
fun supportsDynamicTheming(): Boolean = Build.VERSION.SDK_INT >= Build.VERSION_CODES.S
