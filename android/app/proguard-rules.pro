# wireguard-android reaches the Go backend through JNI and reflection-like lookups.
-keep class com.wireguard.android.backend.** { *; }
-keep class com.wireguard.crypto.** { *; }
-dontwarn javax.annotation.**
