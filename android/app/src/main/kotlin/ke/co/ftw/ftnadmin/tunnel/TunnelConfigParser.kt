package ke.co.ftw.ftnadmin.tunnel

import java.util.Base64

/** What the UI may show about the tunnel. Never contains key material. */
data class TunnelSummary(
    val address: String,
    val endpoint: String,
    val allowedIps: List<String>,
)

/** A validated tunnel: [sanitizedConfig] is what gets stored and handed to the backend. */
class ParsedTunnel(val sanitizedConfig: String, val summary: TunnelSummary) {
    override fun toString() = "ParsedTunnel(summary=$summary)"
}

class InvalidTunnelConfigException(message: String) : Exception(message)

/**
 * Parses and validates a wg-quick style client config as printed by `ftnbilling-admin-peer`.
 *
 * Accepted: exactly one `[Interface]` and one `[Peer]`; every AllowedIPs entry inside the admin
 * network 10.98.0.0/24 and at least one covering the server 10.98.0.1. DNS lines are dropped
 * (the app must never change the device resolver). Anything else is rejected with a clear message.
 */
object TunnelConfigParser {
    const val SERVER_ADDRESS = "10.98.0.1"
    private const val NETWORK = "10.98.0.0"
    private const val MIN_PREFIX = 24

    private val interfaceKeys = setOf("privatekey", "address", "dns", "mtu", "listenport")
    private val peerKeys = setOf("publickey", "presharedkey", "endpoint", "allowedips", "persistentkeepalive")
    private val multiValueKeys = setOf("address", "allowedips", "dns")

    private class Section(val name: String) {
        val values = LinkedHashMap<String, String>()
    }

    @Throws(InvalidTunnelConfigException::class)
    fun parse(text: String): ParsedTunnel {
        val sections = readSections(text)
        val interfaces = sections.filter { it.name == "interface" }
        val peers = sections.filter { it.name == "peer" }
        if (interfaces.size != 1) fail("The config must contain exactly one [Interface] section.")
        if (peers.isEmpty()) fail("The config has no [Peer] section.")
        if (peers.size > 1) fail("The config has ${peers.size} peers; exactly one is allowed.")
        val iface = interfaces.single().values
        val peer = peers.single().values

        for (k in iface.keys) if (k !in interfaceKeys) fail("Unsupported [Interface] setting: ${displayKey(k)}.")
        for (k in peer.keys) if (k !in peerKeys) fail("Unsupported [Peer] setting: ${displayKey(k)}.")

        val privateKey = iface["privatekey"] ?: fail("The config has no PrivateKey.")
        requireKey(privateKey, "PrivateKey")
        val addresses = splitList(iface["address"] ?: fail("The config has no Address."))
        if (addresses.isEmpty()) fail("The Address line is empty.")
        for (a in addresses) {
            val cidr = Ipv4Cidr.parse(a) ?: fail("Address '$a' is not an IPv4 address.")
            if (!cidr.isInsideAdminNetwork()) fail("Address '$a' is outside the admin network $NETWORK/$MIN_PREFIX.")
        }
        iface["mtu"]?.let { if (it.toIntOrNull() !in 576..9000) fail("MTU '$it' is not valid.") }
        iface["listenport"]?.let { if (it.toIntOrNull() !in 1..65535) fail("ListenPort '$it' is not valid.") }

        val publicKey = peer["publickey"] ?: fail("The config has no peer PublicKey.")
        requireKey(publicKey, "PublicKey")
        peer["presharedkey"]?.let { requireKey(it, "PresharedKey") }
        val endpoint = peer["endpoint"] ?: fail("The config has no peer Endpoint.")
        requireEndpoint(endpoint)
        peer["persistentkeepalive"]?.let {
            if (it.toIntOrNull() !in 0..65535) fail("PersistentKeepalive '$it' is not valid.")
        }

        val allowed = splitList(peer["allowedips"] ?: fail("The config has no AllowedIPs."))
        if (allowed.isEmpty()) fail("The AllowedIPs line is empty.")
        val server = Ipv4Cidr.parse(SERVER_ADDRESS)!!
        var coversServer = false
        for (a in allowed) {
            val cidr = Ipv4Cidr.parse(a) ?: fail("AllowedIPs entry '$a' is not an IPv4 network.")
            if (!cidr.isInsideAdminNetwork()) {
                fail("AllowedIPs entry '$a' is outside the admin network $NETWORK/$MIN_PREFIX. This app only routes admin traffic.")
            }
            if (cidr.contains(server)) coversServer = true
        }
        if (!coversServer) fail("AllowedIPs must include the server address $SERVER_ADDRESS/32.")

        val sanitized = buildString {
            append("[Interface]\n")
            append("PrivateKey = ").append(privateKey).append('\n')
            append("Address = ").append(addresses.joinToString(", ")).append('\n')
            iface["mtu"]?.let { append("MTU = ").append(it).append('\n') }
            iface["listenport"]?.let { append("ListenPort = ").append(it).append('\n') }
            append("\n[Peer]\n")
            append("PublicKey = ").append(publicKey).append('\n')
            peer["presharedkey"]?.let { append("PresharedKey = ").append(it).append('\n') }
            append("Endpoint = ").append(endpoint).append('\n')
            append("AllowedIPs = ").append(allowed.joinToString(", ")).append('\n')
            peer["persistentkeepalive"]?.let { append("PersistentKeepalive = ").append(it).append('\n') }
        }
        return ParsedTunnel(sanitized, TunnelSummary(addresses.joinToString(", "), endpoint, allowed))
    }

    private fun readSections(text: String): List<Section> {
        val sections = ArrayList<Section>()
        var current: Section? = null
        for (raw in text.lineSequence()) {
            val line = raw.substringBefore('#').trim()
            if (line.isEmpty()) continue
            if (line.startsWith("[")) {
                if (!line.endsWith("]")) fail("Unreadable line: '${shorten(line)}'.")
                val name = line.substring(1, line.length - 1).trim().lowercase()
                if (name != "interface" && name != "peer") fail("Unknown section [${shorten(name)}].")
                current = Section(name).also { sections.add(it) }
                continue
            }
            val section = current ?: fail("This does not look like a WireGuard config.")
            val eq = line.indexOf('=')
            if (eq <= 0) fail("Unreadable line: '${shorten(line)}'.")
            val key = line.substring(0, eq).trim().lowercase()
            val value = line.substring(eq + 1).trim()
            if (key in section.values) {
                if (key !in multiValueKeys) fail("${displayKey(key)} is set twice.")
                section.values[key] = section.values.getValue(key) + "," + value
            } else {
                section.values[key] = value
            }
        }
        if (sections.isEmpty()) fail("This does not look like a WireGuard config.")
        return sections
    }

    private fun splitList(value: String) = value.split(',').map { it.trim() }.filter { it.isNotEmpty() }

    private fun requireKey(value: String, label: String) {
        val bytes = try {
            Base64.getDecoder().decode(value)
        } catch (_: IllegalArgumentException) {
            null
        }
        if (bytes == null || bytes.size != 32) fail("$label is not a valid WireGuard key.")
    }

    private fun requireEndpoint(value: String) {
        val colon = value.lastIndexOf(':')
        val host = if (colon > 0) value.substring(0, colon).removeSurrounding("[", "]") else ""
        val port = if (colon > 0) value.substring(colon + 1).toIntOrNull() else null
        if (host.isEmpty() || host.any { it.isWhitespace() } || port == null || port !in 1..65535) {
            fail("Endpoint '${shorten(value)}' must look like host:port.")
        }
    }

    private fun displayKey(lower: String) = lower.replaceFirstChar { it.uppercase() }

    private fun shorten(s: String) = if (s.length > 24) s.take(24) + "..." else s

    private fun fail(message: String): Nothing = throw InvalidTunnelConfigException(message)

    /** IPv4 address with prefix length. */
    private class Ipv4Cidr(val ip: Long, val prefix: Int) {
        private val mask get() = if (prefix == 0) 0L else (0xFFFFFFFFL shl (32 - prefix)) and 0xFFFFFFFFL

        fun contains(other: Ipv4Cidr) = prefix <= other.prefix && (other.ip and mask) == (ip and mask)

        fun isInsideAdminNetwork(): Boolean = prefix >= MIN_PREFIX && ADMIN_NET.contains(this)

        companion object {
            val ADMIN_NET = Ipv4Cidr(octets(NETWORK)!!, MIN_PREFIX)

            fun parse(text: String): Ipv4Cidr? {
                val slash = text.indexOf('/')
                val ip = octets(if (slash < 0) text else text.substring(0, slash)) ?: return null
                val prefix = if (slash < 0) 32 else text.substring(slash + 1).toIntOrNull() ?: return null
                if (prefix !in 0..32) return null
                return Ipv4Cidr(ip, prefix)
            }

            private fun octets(text: String): Long? {
                val parts = text.split('.')
                if (parts.size != 4) return null
                var v = 0L
                for (p in parts) {
                    val n = p.toIntOrNull() ?: return null
                    if (n !in 0..255 || p.length > 3) return null
                    v = (v shl 8) or n.toLong()
                }
                return v
            }
        }
    }
}
