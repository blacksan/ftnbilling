package ke.co.ftw.ftnadmin.core.network

import kotlinx.serialization.json.Json

/** Lenient reader: the server may add fields; absent ones fall back to defaults. */
val ApiJson: Json = Json {
    ignoreUnknownKeys = true
    coerceInputValues = true
    explicitNulls = false
    encodeDefaults = false
    isLenient = false
}
