import CryptoKit
import Foundation
import HTTPTypes
import OpenAPIRuntime

/// Signs data with a device's P-256 request key (docs/specs/devices.md). The key lives in the
/// Secure Enclave on a phone; tests and the simulator use a software key.
public protocol RequestSigner: Sendable {
    /// The public key as JWK coordinates, unpadded base64url.
    var publicJWK: (x: String, y: String) { get }
    /// An ECDSA P-256 SHA-256 signature over `data`, as the raw 64-byte r‖s that JWS uses.
    func sign(_ data: Data) throws -> Data
}

/// A software P-256 key: tests, and the simulator, which has no Secure Enclave.
public struct SoftwareSigner: RequestSigner {
    public let key: P256.Signing.PrivateKey

    public init(key: P256.Signing.PrivateKey = .init()) { self.key = key }

    public var publicJWK: (x: String, y: String) { jwkCoordinates(key.publicKey.rawRepresentation) }

    public func sign(_ data: Data) throws -> Data { try key.signature(for: data).rawRepresentation }
}

/// A key in the Secure Enclave. Its `dataRepresentation` is an opaque blob only this phone's
/// Secure Enclave can use; the private key never leaves it.
public struct SecureEnclaveSigner: RequestSigner {
    public let key: SecureEnclave.P256.Signing.PrivateKey

    public init(key: SecureEnclave.P256.Signing.PrivateKey) { self.key = key }

    public var publicJWK: (x: String, y: String) { jwkCoordinates(key.publicKey.rawRepresentation) }

    public func sign(_ data: Data) throws -> Data { try key.signature(for: data).rawRepresentation }
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
    public static let audience = "mainloop-api"
    public static let type = "mainloop-device+jwt"
    /// The token's lifetime; the server refuses more than 60 seconds.
    public static let lifetime: TimeInterval = 60

    /// - Parameters:
    ///   - target: the path and query the API sees, relative to its base URL (`/attention?x=1`).
    public static func make(
        deviceID: String,
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

    private static func segment(_ object: [String: any Sendable]) throws -> String {
        Base64URL.encode(try JSONSerialization.data(withJSONObject: object, options: [.sortedKeys, .withoutEscapingSlashes]))
    }
}

/// Signs every request with the device's key. The body is collected (the API caps bodies at
/// 8 MiB) so its hash can be signed, then sent as collected.
public struct DeviceSigningMiddleware: ClientMiddleware {
    public static let maxBody = 8 << 20

    let deviceID: String
    let signer: any RequestSigner
    let now: @Sendable () -> Date

    public init(deviceID: String, signer: any RequestSigner, now: @escaping @Sendable () -> Date = { Date() }) {
        self.deviceID = deviceID
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
            deviceID: deviceID, method: request.method.rawValue, target: request.path ?? "/",
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
