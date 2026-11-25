import Foundation
import MainloopAPI

/// The device's web session, which the app's web view sends as a cookie (docs/specs/devices.md,
/// "The app's web view"). One request signed with the device key starts it; the token never
/// reaches the page's scripts.
public struct WebSessionGrant: Equatable, Sendable {
    /// The server's name for the cookie. `__Host-`: Secure, host-only, path `/`.
    public static let cookieName = "__Host-mainloop-device"
    /// Start a new session when less than this is left.
    public static let renewWithin: TimeInterval = 10 * 60

    public let token: String
    public let expiresAt: Date

    public init(token: String, expiresAt: Date) {
        self.token = token
        self.expiresAt = expiresAt
    }

    public func needsRenewal(at now: Date = Date()) -> Bool {
        expiresAt.timeIntervalSince(now) < Self.renewWithin
    }

    /// The cookie for `origin`'s host: HttpOnly, Secure, SameSite=Strict, path `/`, no Domain
    /// (host-only), and no expiry, so it lives only as long as the app keeps it; the server ends
    /// the session on its own after an hour.
    public func cookie(for origin: URL) -> HTTPCookie? {
        guard let host = origin.host(), !host.isEmpty else { return nil }
        return HTTPCookie(properties: [
            .name: Self.cookieName,
            .value: token,
            .domain: host,
            .path: "/",
            .secure: "TRUE",
            HTTPCookiePropertyKey("HttpOnly"): "TRUE",
            .sameSitePolicy: HTTPCookieStringPolicy.sameSiteStrict,
        ])
    }

    /// `POST /devices/current/web-session`. The client must sign with the device key; a web
    /// session's own request is refused.
    public static func start(client: any APIProtocol) async throws -> WebSessionGrant {
        switch try await client.createDeviceWebSession() {
        case .created(let created):
            let session = try created.body.json
            guard session.cookieName == cookieName, isToken(session.token) else { throw WebSessionError.malformed }
            return WebSessionGrant(token: session.token, expiresAt: session.expiresAt)
        case .unauthorized, .forbidden: throw WebSessionError.notPaired
        case .undocumented(let status, _): throw WebSessionError.unexpectedStatus(status)
        }
    }

    /// `DELETE /devices/current/web-session`, best effort: signing out also revokes the device,
    /// which ends its sessions.
    public static func end(client: any APIProtocol) async {
        _ = try? await client.endDeviceWebSessions()
    }

    /// base64url, 32 to 128 characters, as the contract says.
    static func isToken(_ token: String) -> Bool {
        (32...128).contains(token.utf8.count)
            && token.unicodeScalars.allSatisfy { ("A"..."Z").contains($0) || ("a"..."z").contains($0) || ("0"..."9").contains($0) || $0 == "-" || $0 == "_" }
    }
}

public enum WebSessionError: Error, Equatable, Sendable {
    /// The server doesn't know this device (revoked elsewhere, or the keys are gone): pair again.
    case notPaired
    case malformed
    case unexpectedStatus(Int)
}

/// A message from the web app to the app (web/src/lib/nativeShell.ts). Anything else is ignored.
public enum ShellMessage: Equatable, Sendable {
    case approve(ApprovalRequest)
    case sessionExpired
    case openDeviceSettings

    /// The handler's name, `window.webkit.messageHandlers.mainloop`.
    public static let handlerName = "mainloop"

    /// Reads a message's body as WebKit hands it over. An `approve` uses only the item and action
    /// ids; a digest the page sends is ignored (the phone hashes the record itself).
    public init?(body: Any) {
        guard let body = body as? [String: Any], let type = body["type"] as? String else { return nil }
        switch type {
        case "approve":
            guard let item = body["item_id"] as? String, let action = body["action_id"] as? String,
                  let request = ApprovalRequest(queueItemID: item, actionID: action)
            else { return nil }
            self = .approve(request)
        case "sessionExpired": self = .sessionExpired
        case "openDeviceSettings": self = .openDeviceSettings
        default: return nil
        }
    }
}

/// What the web view does with a navigation.
public enum ShellNavigation: Equatable, Sendable {
    /// Load it in the web view.
    case load
    /// Open it in Safari, outside the app's web view and its bridge.
    case openOutside
    /// Drop it.
    case refuse

    /// The main frame stays on Mainloop's origin: any other web page opens in Safari, and anything
    /// else (`javascript:`, `data:`, other schemes) is refused. A frame inside a page (a workspace
    /// preview) may load any web page; the bridge never answers a frame.
    public static func decide(_ url: URL, origin: URL, mainFrame: Bool) -> ShellNavigation {
        if isSameOrigin(url, origin) { return .load }
        let scheme = url.scheme?.lowercased()
        if !mainFrame {
            return scheme == "https" || scheme == "http" || scheme == "about" ? .load : .refuse
        }
        return scheme == "https" || scheme == "http" ? .openOutside : .refuse
    }

    /// Same scheme, host and port, the web's own rule.
    public static func isSameOrigin(_ url: URL, _ origin: URL) -> Bool {
        guard let scheme = url.scheme?.lowercased(), scheme == origin.scheme?.lowercased(),
              let host = url.host()?.lowercased(), host == origin.host()?.lowercased()
        else { return false }
        return port(url) == port(origin)
    }

    /// The security origin WebKit reports for a frame (WKSecurityOrigin's parts), checked the same way.
    public static func isOrigin(scheme: String, host: String, port: Int, of origin: URL) -> Bool {
        var parts = URLComponents()
        parts.scheme = scheme
        parts.host = host
        if port != 0 { parts.port = port }
        guard let url = parts.url else { return false }
        return isSameOrigin(url, origin)
    }

    private static func port(_ url: URL) -> Int? {
        if let port = url.port { return port }
        switch url.scheme?.lowercased() {
        case "https": return 443
        case "http": return 80
        default: return nil
        }
    }
}

extension PushRoute {
    /// The web app's page for this notification: the inbox, with the card in view when there is
    /// one (`/inbox?item=<queue item id>`), or the task.
    public var webPath: String {
        switch self {
        case .needsYou:
            return "/inbox"
        case .needsYouItem(let id):
            // An attention item id is `queue:<queue item id>`; the inbox finds the card by the latter.
            guard id.hasPrefix("queue:"), let item = String(id.dropFirst(6)).addingPercentEncoding(withAllowedCharacters: .alphanumerics.union(.init(charactersIn: "-._~"))),
                  !item.isEmpty
            else { return "/inbox" }
            return "/inbox?item=" + item
        case .webPage(let path):
            return path
        }
    }
}

/// The number on the app icon: Needs you's count, as the server's notifications set it.
public enum AttentionBadge {
    public static func count(client: any APIProtocol) async throws -> Int {
        switch try await client.getAttention() {
        case .ok(let ok): return try ok.body.json.count
        case .undocumented(let status, _): throw WebSessionError.unexpectedStatus(status)
        default: throw WebSessionError.unexpectedStatus(0)
        }
    }
}
