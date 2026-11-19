import org.gradle.api.JavaVersion

plugins {
    alias(libs.plugins.android.application)
    alias(libs.plugins.kotlin.compose)
    alias(libs.plugins.kotlin.serialization)
}

// Release signing comes from the environment (CI). Without it the release build stays unsigned.
val signingPath: String? = providers.environmentVariable("ANDROID_KEYSTORE_PATH").orNull

// CI keeps the signing secrets in a 0600 file (decrypted from android/signing) and passes only its path,
// so the passwords are never environment variables. Lines look like ANDROID_KEY_ALIAS=value.
val signingFile: Map<String, String> = providers.environmentVariable("ANDROID_SIGNING_ENV_FILE").orNull
    ?.let { path ->
        file(path).readLines().mapNotNull { raw ->
            val line = raw.trimEnd('\r')
            val i = line.indexOf('=')
            if (i <= 0) {
                null
            } else {
                var v = line.substring(i + 1)
                if (v.length >= 2 && (v.first() == '"' || v.first() == '\'') && v.last() == v.first()) v = v.substring(1, v.length - 1)
                line.substring(0, i) to v
            }
        }.toMap()
    }
    ?: emptyMap()

fun signingValue(name: String): String? = signingFile[name] ?: providers.environmentVariable(name).orNull

android {
    namespace = "ke.co.ftw.ftnadmin"
    compileSdk = 37

    defaultConfig {
        applicationId = "ke.co.ftw.ftnadmin"
        minSdk = 26
        targetSdk = 36
        versionCode = providers.gradleProperty("versionCode").map(String::toInt).getOrElse(1)
        versionName = providers.gradleProperty("versionName").getOrElse("0.1.0-dev")

        buildConfigField("String", "API_BASE", "\"https://isp.ftw.co.ke\"")
        // The in-app update check is for signed release builds; the .debug app is installed by hand.
        buildConfigField("boolean", "UPDATES_ENABLED", "true")
    }

    signingConfigs {
        if (signingPath != null) {
            create("release") {
                storeFile = file(signingPath)
                storePassword = signingValue("ANDROID_KEYSTORE_PASSWORD")
                keyAlias = signingValue("ANDROID_KEY_ALIAS")
                keyPassword = signingValue("ANDROID_KEY_PASSWORD")
                // AGP leaves v1 (JAR) signing off for minSdk 24+. Android 8.0/8.1 phones (minSdk is 26) need it
                // to read the signer of a downloaded update, so it is switched on explicitly.
                enableV1Signing = true
                enableV2Signing = true
            }
        }
    }

    buildTypes {
        debug {
            applicationIdSuffix = ".debug"
            versionNameSuffix = "-debug"
            buildConfigField("boolean", "UPDATES_ENABLED", "false")
            // Debug builds may point at a staging server: -PapiBase=https://staging.example (https only).
            val debugBase = providers.gradleProperty("apiBase").getOrElse("https://isp.ftw.co.ke")
            require(debugBase.startsWith("https://")) { "apiBase must be https" }
            buildConfigField("String", "API_BASE", "\"$debugBase\"")
        }
        release {
            isMinifyEnabled = true
            isShrinkResources = true
            proguardFiles(getDefaultProguardFile("proguard-android-optimize.txt"), "proguard-rules.pro")
            signingConfig = signingConfigs.findByName("release")
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    buildFeatures {
        compose = true
        buildConfig = true
    }

    lint {
        textReport = true
    }

    testOptions {
        unitTests.isReturnDefaultValues = true
    }

    packaging {
        resources.excludes += "/META-INF/{AL2.0,LGPL2.1}"
    }
}

dependencies {
    implementation(platform(libs.compose.bom))
    implementation(libs.compose.ui)
    implementation(libs.compose.ui.tooling.preview)
    implementation(libs.compose.material3)
    implementation(libs.compose.material3.navigation.suite)
    implementation(libs.compose.material.icons.core)
    debugImplementation(libs.compose.ui.tooling)

    implementation(libs.androidx.activity.compose)
    implementation(libs.androidx.core.ktx)
    implementation(libs.androidx.core.splashscreen)
    implementation(libs.androidx.appcompat)
    implementation(libs.androidx.lifecycle.runtime.compose)
    implementation(libs.androidx.lifecycle.viewmodel.compose)
    implementation(libs.androidx.lifecycle.process)
    implementation(libs.androidx.navigation.compose)
    implementation(libs.androidx.datastore.preferences)
    implementation(libs.androidx.biometric)
    implementation(libs.kotlinx.coroutines.android)

    implementation(libs.okhttp)
    implementation(libs.kotlinx.serialization.json)

    testImplementation(libs.junit)
    testImplementation(libs.okhttp.mockwebserver)
    testImplementation(libs.kotlinx.coroutines.test)
}
