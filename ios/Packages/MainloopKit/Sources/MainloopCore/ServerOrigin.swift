import Foundation

/// The one Mainloop origin the app talks to, from the build setting `MAINLOOP_ORIGIN`
/// (Info.plist key `MainloopOrigin`). HTTPS only, except loopback for a local server. A
/// placeholder origin (`*.example.test`, `*.example`), an empty one or an invalid one means the
/// build names no server, and the app says how to set one.
public enum ServerOrigin: Equatable, Sendable {
    case live(URL)
    case notConfigured

    public static let infoPlistKey = "MainloopOrigin"

    public static func parse(_ raw: String?) -> ServerOrigin {
        guard let raw = raw?.trimmingCharacters(in: .whitespaces), !raw.isEmpty,
              let url = URL(string: raw), let host = url.host(), !host.isEmpty,
              url.user == nil, url.password == nil, url.query == nil, url.fragment == nil,
              url.path.isEmpty || url.path == "/"
        else { return .notConfigured }
        let lower = host.lowercased()
        if lower.hasSuffix(".example.test") || lower.hasSuffix(".example") || lower == "example.test" {
            return .notConfigured
        }
        let loopback = lower == "localhost" || lower == "127.0.0.1" || lower == "::1" || lower == "[::1]"
        switch url.scheme?.lowercased() {
        case "https": break
        case "http" where loopback: break
        default: return .notConfigured
        }
        // Rebuild from the parsed parts, keeping the host as written (an IPv6 literal keeps
        // its brackets), lowercased.
        guard var parts = URLComponents(url: url, resolvingAgainstBaseURL: false) else { return .notConfigured }
        parts.scheme = parts.scheme?.lowercased()
        parts.percentEncodedHost = parts.percentEncodedHost?.lowercased()
        parts.path = ""
        return parts.url.map(ServerOrigin.live) ?? .notConfigured
    }
}
