import CryptoKit
import Foundation
import HTTPTypes
import LocalAuthentication
import OpenAPIRuntime

/// A device's P-256 public key.
public protocol DevicePublicKey: Sendable {
    /// The public key as JWK coordinates, unpadded base64url.
    var publicJWK: (x: String, y: String) { get }
}

/// Signs data with a device's P-256 request key (docs/specs/devices.md). The key lives in the
/// Secure Enclave on a phone; tests and the simulator use a software key.
public protocol RequestSigner: DevicePublicKey {
    /// An ECDSA P-256 SHA-256 signature over `data`, as the raw 64-byte r‖s that JWS uses.
    func sign(_ data: Data) throws -> Data
}

/// Signs approvals of privileged actions with a device's second key, the approval key. On a
/// phone it is a Secure Enclave key that asks for Touch ID or Face ID at every signature; tests
/// and the simulator use a software key, which asks for nothing.
public protocol ApprovalSigner: DevicePublicKey {
    /// An ECDSA P-256 SHA-256 signature over `data`, raw r‖s. `reason` is what the biometric
    /// prompt says is being approved. Throws `ApprovalSigningError.cancelled` when the owner
    /// cancels the prompt.
    func sign(_ data: Data, reason: String) async throws -> Data
}

public enum ApprovalSigningError: Error, Equatable, Sendable {
    /// The owner cancelled the prompt, or the system did: not an error to show.
    case cancelled
    /// Touch ID or Face ID didn't recognise the owner.
    case notRecognised
    /// No biometry enrolled, locked out, or not available on this phone.
    case biometryUnavailable
    /// The Secure Enclave refused the key, as it does once the enrolled fingers or face change.
    case keyUnusable
}

/// A software P-256 key: tests, and the simulator, which has no Secure Enclave.
public struct SoftwareSigner: RequestSigner {
    public let key: P256.Signing.PrivateKey

    public init(key: P256.Signing.PrivateKey = .init()) { self.key = key }

    public var publicJWK: (x: String, y: String) { jwkCoordinates(key.publicKey.rawRepresentation) }

    public func sign(_ data: Data) throws -> Data { try key.signature(for: data).rawRepresentation }
}

extension SoftwareSigner: ApprovalSigner {
    public func sign(_ data: Data, reason _: String) async throws -> Data { try sign(data) }
}

/// A key in the Secure Enclave. Its `dataRepresentation` is an opaque blob only this phone's
/// Secure Enclave can use; the private key never leaves it.
public struct SecureEnclaveSigner: RequestSigner {
    public let key: SecureEnclave.P256.Signing.PrivateKey

    public init(key: SecureEnclave.P256.Signing.PrivateKey) { self.key = key }

    public var publicJWK: (x: String, y: String) { jwkCoordinates(key.publicKey.rawRepresentation) }

    public func sign(_ data: Data) throws -> Data { try key.signature(for: data).rawRepresentation }
}

/// The approval key in the Secure Enclave, made with an access control that requires the current
/// set of enrolled fingers or face for every use (`.biometryCurrentSet`): re-enrolling makes it
/// unusable. Each signature asks for Touch ID or Face ID with `reason`.
public struct SecureEnclaveApprovalSigner: ApprovalSigner {
    /// The key's opaque Secure Enclave blob.
    public let keyData: Data
    public let publicJWK: (x: String, y: String)

    public init(keyData: Data) throws {
        self.keyData = keyData
        publicJWK = try jwkCoordinates(SecureEnclave.P256.Signing.PrivateKey(dataRepresentation: keyData).publicKey.rawRepresentation)
    }

    public func sign(_ data: Data, reason: String) async throws -> Data {
        let context = LAContext()
        context.localizedReason = reason
        // Only the enrolled fingers or face: no passcode fallback, which the key would refuse.
        context.localizedFallbackTitle = ""
        do {
            _ = try await context.evaluatePolicy(.deviceOwnerAuthenticationWithBiometrics, localizedReason: reason)
        } catch let error as LAError {
            switch error.code {
            case .userCancel, .appCancel, .systemCancel, .userFallback: throw ApprovalSigningError.cancelled
            case .authenticationFailed: throw ApprovalSigningError.notRecognised
            default: throw ApprovalSigningError.biometryUnavailable
            }
        }
        // The context has just been evaluated, so using the key doesn't ask again.
        do {
            let key = try SecureEnclave.P256.Signing.PrivateKey(dataRepresentation: keyData, authenticationContext: context)
            return try key.signature(for: data).rawRepresentation
        } catch {
            throw ApprovalSigningError.keyUnusable
        }
    }
}

public extension DevicePublicKey {
    /// The key's RFC 7638 thumbprint: base64url SHA-256 of its canonical JWK. The sign-in that
    /// registers the key carries it as the nonce, so that sign-in registers this key only.
    var thumbprint: String {
        let jwk = publicJWK
        let canonical = #"{"crv":"P-256","kty":"EC","x":"\#(jwk.x)","y":"\#(jwk.y)"}"#
        return Base64URL.encode(Data(SHA256.hash(data: Data(canonical.utf8))))
    }
}

/// x and y from a P-256 raw public key (64 bytes, x‖y).
func jwkCoordinates(_ raw: Data) -> (x: String, y: String) {
    (Base64URL.encode(raw.prefix(32)), Base64URL.encode(raw.suffix(32)))
}

public enum Base64URL {
    public static func encode(_ data: some DataProtocol) -> String {
        Data(data).base64EncodedString()
            .replacingOccurrences(of: "+", with: "-")
            .replacingOccurrences(of: "/", with: "_")
            .replacingOccurrences(of: "=", with: "")
    }

    public static func decode(_ string: String) -> Data? {
        var s = string.replacingOccurrences(of: "-", with: "+").replacingOccurrences(of: "_", with: "/")
        while s.count % 4 != 0 { s += "=" }
        return Data(base64Encoded: s)
    }
}

/// A device request token: an ES256 JWT bound to one request (docs/specs/devices.md).
public enum DeviceToken {
    public static let issuer = "mainloop-device"
    public static let type = "mainloop-device+jwt"
    /// The token's lifetime; the server refuses more than 60 seconds.
    public static let lifetime: TimeInterval = 60

    /// The audience for an instance: its origin, `scheme://host[:port]`, lowercased, the way the
    /// server names itself. A token signed for one instance is refused by another.
    public static func audience(for origin: URL) -> String {
        var c = URLComponents()
        c.scheme = origin.scheme?.lowercased()
        c.host = origin.host?.lowercased()
        c.port = origin.port
        return c.string ?? origin.absoluteString.lowercased()
    }

    /// - Parameters:
    ///   - audience: the instance's origin (`audience(for:)`).
    ///   - target: the path and query the API sees, relative to its base URL (`/attention?x=1`).
    public static func make(
        deviceID: String,
        audience: String,
        method: String,
        target: String,
        body: Data,
        signer: some RequestSigner,
        now: Date = Date(),
        jti: String = randomID()
    ) throws -> String {
        let iat = Int(now.timeIntervalSince1970)
        let header: [String: String] = ["alg": "ES256", "typ": type, "kid": deviceID]
        let claims: [String: any Sendable] = [
            "iss": issuer, "sub": deviceID, "aud": audience,
            "iat": iat, "exp": iat + Int(lifetime), "jti": jti,
            "htm": method, "htu": target, "bh": Base64URL.encode(Data(SHA256.hash(data: body))),
        ]
        let input = try segment(header) + "." + segment(claims)
        let signature = try signer.sign(Data(input.utf8))
        return input + "." + Base64URL.encode(signature)
    }

    /// 32 hex characters from the system's random generator.
    public static func randomID() -> String {
        var bytes = [UInt8](repeating: 0, count: 16)
        for i in bytes.indices { bytes[i] = UInt8.random(in: .min ... .max) }
        return bytes.map { String(format: "%02x", $0) }.joined()
    }

    static func segment(_ object: [String: any Sendable]) throws -> String {
        Base64URL.encode(try JSONSerialization.data(withJSONObject: object, options: [.sortedKeys, .withoutEscapingSlashes]))
    }
}

/// An approval of one privileged action, signed with the device's approval key: an ES256 JWT
/// that the server checks against the key registered with the device, and the action's id and
/// digest (docs/specs/privileged-actions.md, "Decision").
public enum ApprovalToken {
    public static let type = "mainloop-approval+jwt"
    /// The token's lifetime.
    public static let lifetime: TimeInterval = 120

    /// - Parameters:
    ///   - audience: the instance's origin (`DeviceToken.audience(for:)`).
    ///   - actionID: the card's `privileged_action_id`.
    ///   - digest: the card's `privileged_action_digest`, 64 lowercase hex characters.
    ///   - reason: what the biometric prompt says, such as "Approve: <card title>".
    public static func make(
        deviceID: String,
        audience: String,
        actionID: String,
        digest: String,
        signer: some ApprovalSigner,
        reason: String,
        now: Date = Date(),
        jti: String = DeviceToken.randomID()
    ) async throws -> String {
        let iat = Int(now.timeIntervalSince1970)
        let header: [String: String] = ["alg": "ES256", "typ": type, "kid": deviceID]
        let claims: [String: any Sendable] = [
            "iss": DeviceToken.issuer, "sub": deviceID, "aud": audience,
            "iat": iat, "exp": iat + Int(lifetime), "jti": jti,
            "act": actionID, "dig": digest,
        ]
        let input = try DeviceToken.segment(header) + "." + DeviceToken.segment(claims)
        let signature = try await signer.sign(Data(input.utf8), reason: reason)
        return input + "." + Base64URL.encode(signature)
    }
}

/// Signs every request with the device's key. The body is collected (the API caps bodies at
/// 8 MiB) so its hash can be signed, then sent as collected.
public struct DeviceSigningMiddleware: ClientMiddleware {
    public static let maxBody = 8 << 20

    let deviceID: String
    let audience: String
    let signer: any RequestSigner
    let now: @Sendable () -> Date

    /// - Parameter origin: the instance the device is registered with; tokens name it.
    public init(deviceID: String, origin: URL, signer: any RequestSigner, now: @escaping @Sendable () -> Date = { Date() }) {
        self.deviceID = deviceID
        self.audience = DeviceToken.audience(for: origin)
        self.signer = signer
        self.now = now
    }

    public func intercept(
        _ request: HTTPRequest,
        body: HTTPBody?,
        baseURL: URL,
        operationID: String,
        next: @Sendable (HTTPRequest, HTTPBody?, URL) async throws -> (HTTPResponse, HTTPBody?)
    ) async throws -> (HTTPResponse, HTTPBody?) {
        var request = request
        let data: Data = if let body { try await Data(collecting: body, upTo: Self.maxBody) } else { Data() }
        let token = try DeviceToken.make(
            deviceID: deviceID, audience: audience, method: request.method.rawValue, target: request.path ?? "/",
            body: data, signer: signer, now: now()
        )
        request.headerFields[.authorization] = "Bearer " + token
        return try await next(request, body == nil ? nil : HTTPBody(data), baseURL)
    }
}

/// Sends a fixed bearer: the Pocket ID token, for registering the device only.
public struct BearerMiddleware: ClientMiddleware {
    let token: String

    public init(token: String) { self.token = token }

    public func intercept(
        _ request: HTTPRequest,
        body: HTTPBody?,
        baseURL: URL,
        operationID: String,
        next: @Sendable (HTTPRequest, HTTPBody?, URL) async throws -> (HTTPResponse, HTTPBody?)
    ) async throws -> (HTTPResponse, HTTPBody?) {
        var request = request
        request.headerFields[.authorization] = "Bearer " + token
        return try await next(request, body, baseURL)
    }
}
