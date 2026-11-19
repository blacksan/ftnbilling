// Top-level build file. Kotlin is built into AGP 9, so only the Compose compiler plugin is applied.
plugins {
    alias(libs.plugins.android.application) apply false
    alias(libs.plugins.kotlin.compose) apply false
    alias(libs.plugins.kotlin.serialization) apply false
}
