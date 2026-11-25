import Foundation
import MainloopAPI
import OpenAPIRuntime

/// APNs device tokens as the server takes them (`DevicePushRegistration.token`).
public enum APNsToken {
    /// Lower-case hex, two characters per byte.
    public static func hex(_ data: Data) -> String {
        let digits = Array("0123456789abcdef".utf8)
        var out = [UInt8]()
        out.reserveCapacity(data.count * 2)
        for byte in data {
            out.append(digits[Int(byte >> 4)])
            out.append(digits[Int(byte & 0x0F)])
        }
        return String(decoding: out, as: UTF8.self)
    }
}

public enum PushError: Error, Equatable, Sendable {
    /// A 401: the phone isn't registered (or its key was revoked).
    case signedOut
    /// The server refused the token (403: not a device's request; 404: the device is gone;
    /// 422: a malformed token), or answered something undocumented.
    case refused(Int)
}

/// Sends this phone's APNs token to Mainloop (`PUT /devices/current/push`), signed with the
/// device's request key. Setting the same token again is harmless, so the app sends it on every
/// launch while signed in and whenever iOS hands it a token.
public enum PushRegistration {
    /// Info.plist key carrying the build's `aps-environment` (`MAINLOOP_APNS_ENVIRONMENT`).
    public static let environmentInfoPlistKey = "MainloopAPNsEnvironment"

    /// The environment the build's entitlement names; nil if the setting is missing or unknown,
    /// in which case the app doesn't send a token rather than guess.
    public static func environment(infoPlistValue raw: String?) -> Components.Schemas.APNsEnvironment? {
        raw.flatMap { Components.Schemas.APNsEnvironment(rawValue: $0.trimmingCharacters(in: .whitespaces).lowercased()) }
    }

    /// Sets the token and returns the server's view of it.
    @discardableResult
    public static func send(
        token: Data,
        environment: Components.Schemas.APNsEnvironment,
        client: some APIProtocol
    ) async throws -> Components.Schemas.DevicePush? {
        let body = Components.Schemas.DevicePushRegistration(token: APNsToken.hex(token), environment: environment)
        switch try await client.setDevicePush(body: .json(body)) {
        case .ok(let ok): return try ok.body.json.push
        case .unauthorized: throw PushError.signedOut
        case .forbidden: throw PushError.refused(403)
        case .notFound: throw PushError.refused(404)
        case .unprocessableContent: throw PushError.refused(422)
        case .undocumented(let status, _): throw PushError.refused(status)
        }
    }

    /// The server's view of this phone's token (`GET /devices/current`): nil when it has none.
    public static func status(client: some APIProtocol) async throws -> Components.Schemas.DevicePush? {
        switch try await client.getCurrentDevice() {
        case .ok(let ok): return try ok.body.json.push
        case .unauthorized: throw PushError.signedOut
        case .forbidden: throw PushError.refused(403)
        case .notFound: throw PushError.refused(404)
        case .undocumented(let status, _): throw PushError.refused(status)
        }
    }
}

/// Notification categories the server sets in `aps.category`.
public enum PushCategory {
    public static let needsYou = "NEEDS_YOU"
    public static let task = "TASK"
}

/// Where tapping a notification takes the owner.
public enum PushRoute: Equatable, Sendable {
    /// The Needs-you list: for anything the app doesn't recognise.
    case needsYou
    /// One Needs-you card, by its attention item id (`AttentionItem.id`, such as `queue:q1`).
    case needsYouItem(String)
    /// A page of the web app, by path (such as `/tasks/t1`): tasks have no native screen yet.
    case webPage(String)

    /// Reads the `mainloop` object of a notification's payload (docs/specs/push-notifications.md).
    /// Anything missing or malformed falls back to the Needs-you list.
    public init(userInfo: [AnyHashable: Any]) {
        guard let mainloop = userInfo["mainloop"] as? [String: Any] else {
            self = .needsYou
            return
        }
        switch mainloop["type"] as? String {
        case "attention":
            if let item = Self.identifier(mainloop["item_id"]) {
                self = .needsYouItem(item)
            } else {
                self = .needsYou
            }
        case "task":
            if let href = Self.path(mainloop["href"]) {
                self = .webPage(href)
            } else if let task = Self.identifier(mainloop["task_id"]),
                      let escaped = task.addingPercentEncoding(withAllowedCharacters: Self.segment) {
                self = .webPage("/tasks/" + escaped)
            } else {
                self = .needsYou
            }
        default:
            self = .needsYou
        }
    }

    /// The web page's URL on the app's own origin. Only a path is ever taken from a payload, so
    /// a notification can't send the owner to another site.
    public static func url(for path: String, origin: URL) -> URL? {
        guard self.path(path) != nil else { return nil }
        return URL(string: path, relativeTo: origin)?.absoluteURL
    }

    private static let segment: CharacterSet = {
        var set = CharacterSet.urlPathAllowed
        set.remove("/")
        return set
    }()

    /// An id: non-empty, at most 200 visible ASCII characters, no spaces.
    private static func identifier(_ value: Any?) -> String? {
        guard let id = value as? String, !id.isEmpty, id.utf8.count <= 200,
              id.unicodeScalars.allSatisfy({ $0.isASCII && $0.value > 0x20 && $0.value < 0x7F })
        else { return nil }
        return id
    }

    /// An absolute path on the same origin: starts with one `/`, visible ASCII only, no
    /// backslash (which browsers read as `/`), and parses as a URL with no scheme or host.
    private static func path(_ value: Any?) -> String? {
        guard let path = value as? String, path.hasPrefix("/"), !path.hasPrefix("//"), path.utf8.count <= 2048,
              path.unicodeScalars.allSatisfy({ $0.isASCII && $0.value > 0x20 && $0.value < 0x7F && $0 != "\\" }),
              let url = URL(string: path), url.scheme == nil, url.host() == nil
        else { return nil }
        return path
    }
}
