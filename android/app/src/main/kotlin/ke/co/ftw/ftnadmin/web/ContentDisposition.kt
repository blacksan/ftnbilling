package ke.co.ftw.ftnadmin.web

import java.net.URLDecoder

/** Extracts a safe file name from a Content-Disposition header. */
object ContentDisposition {
    private const val DEFAULT = "export.csv"
    private val star = Regex("filename\\*\\s*=\\s*([^']*)'[^']*'([^;]+)", RegexOption.IGNORE_CASE)
    private val quoted = Regex("filename\\s*=\\s*\"([^\"]*)\"", RegexOption.IGNORE_CASE)
    private val bare = Regex("filename\\s*=\\s*([^;\\s]+)", RegexOption.IGNORE_CASE)

    fun fileName(header: String?): String {
        if (header.isNullOrBlank()) return DEFAULT
        val raw = star.find(header)?.let { m ->
            runCatching { URLDecoder.decode(m.groupValues[2].trim().replace("+", "%2B"), m.groupValues[1].ifEmpty { "UTF-8" }) }.getOrNull()
        } ?: quoted.find(header)?.groupValues?.get(1) ?: bare.find(header)?.groupValues?.get(1)
        return sanitize(raw)
    }

    private fun sanitize(name: String?): String {
        val cleaned = name.orEmpty()
            .substringAfterLast('/')
            .substringAfterLast('\\')
            .filter { it.code >= 0x20 && it != ':' && it != '*' && it != '?' && it != '"' && it != '<' && it != '>' && it != '|' }
            .trim()
            .trimStart('.')
        return cleaned.ifEmpty { DEFAULT }.take(120)
    }
}
