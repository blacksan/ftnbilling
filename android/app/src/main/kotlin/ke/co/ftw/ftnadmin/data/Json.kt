package ke.co.ftw.ftnadmin.data

import kotlinx.serialization.json.JsonObjectBuilder
import kotlinx.serialization.json.put

/** Adds a string field only when it has a value. */
fun JsonObjectBuilder.putIfNotBlank(key: String, value: String?) {
    if (!value.isNullOrBlank()) put(key, value)
}
