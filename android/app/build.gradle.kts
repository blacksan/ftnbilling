import org.gradle.api.JavaVersion

plugins {
    alias(libs.plugins.android.application)
    alias(libs.plugins.kotlin.compose)
}

// Release signing comes from the environment (CI). Without it the release build stays unsigned.
val signingPath: String? = providers.environmentVariable("ANDROID_KEYSTORE_PATH").orNull

android {
    namespace = "ke.co.ftw.ftnadmin"
    compileSdk = 36

    defaultConfig {
        applicationId = "ke.co.ftw.ftnadmin"
        minSdk = 26
        targetSdk = 36
        versionCode = providers.gradleProperty("versionCode").map(String::toInt).getOrElse(1)
        versionName = providers.gradleProperty("versionName").getOrElse("0.1.0-dev")

        buildConfigField("String", "ADMIN_ORIGIN", "\"http://10.98.0.1:8080\"")
        buildConfigField("String", "ADMIN_HOST", "\"10.98.0.1\"")
        buildConfigField("int", "ADMIN_PORT", "8080")
        buildConfigField("String", "TUNNEL_LIB_VERSION", "\"${libs.versions.tunnel.get()}\"")
    }

    signingConfigs {
        if (signingPath != null) {
            create("release") {
                storeFile = file(signingPath)
                storePassword = providers.environmentVariable("ANDROID_KEYSTORE_PASSWORD").orNull
                keyAlias = providers.environmentVariable("ANDROID_KEY_ALIAS").orNull
                keyPassword = providers.environmentVariable("ANDROID_KEY_PASSWORD").orNull
            }
        }
    }

    buildTypes {
        debug {
            applicationIdSuffix = ".debug"
            versionNameSuffix = "-debug"
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

    packaging {
        resources.excludes += "/META-INF/{AL2.0,LGPL2.1}"
    }
}

dependencies {
    implementation(platform(libs.compose.bom))
    implementation(libs.compose.ui)
    implementation(libs.compose.ui.tooling.preview)
    implementation(libs.compose.material3)
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

    implementation(libs.wireguard.tunnel)
    implementation(libs.zxing.embedded)

    testImplementation(libs.junit)
    testImplementation(libs.org.json)
    testImplementation(libs.kotlinx.coroutines.test)
}
