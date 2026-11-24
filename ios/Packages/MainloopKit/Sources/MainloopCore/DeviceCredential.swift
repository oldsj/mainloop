import CryptoKit
import Foundation
import MainloopAPI
import HTTPTypes
import OpenAPIRuntime
import Security

/// What the phone keeps after registering (docs/specs/devices.md): its device id, the origin it
/// registered with, and its request key. On a phone the key is a Secure Enclave blob that only
/// this phone can use; in the simulator, which has no Secure Enclave, a software key.
public struct DeviceCredential: Codable, Equatable, Sendable {
    public enum KeyKind: String, Codable, Sendable { case secureEnclave, software }

    public let deviceID: String
    public let origin: URL
    public let keyKind: KeyKind
    public let keyData: Data

    public init(deviceID: String, origin: URL, keyKind: KeyKind, keyData: Data) {
        self.deviceID = deviceID
        self.origin = origin
        self.keyKind = keyKind
        self.keyData = keyData
    }

    public func signer() throws -> any RequestSigner {
        switch keyKind {
        case .secureEnclave: try SecureEnclaveSigner(key: .init(dataRepresentation: keyData))
        case .software: try SoftwareSigner(key: .init(rawRepresentation: keyData))
        }
    }
}

/// A new request key, before it is registered.
public struct NewDeviceKey: Sendable {
    public let kind: DeviceCredential.KeyKind
    public let data: Data
    public let signer: any RequestSigner

    /// A Secure Enclave key, usable after the phone's first unlock, on this phone only. Where
    /// there is no Secure Enclave, a software key only if `allowSoftware` (the simulator).
    public static func make(allowSoftware: Bool) throws -> NewDeviceKey {
        if SecureEnclave.isAvailable {
            var error: Unmanaged<CFError>?
            guard let access = SecAccessControlCreateWithFlags(
                nil, kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly, .privateKeyUsage, &error
            ) else { throw error!.takeRetainedValue() as Error }
            let key = try SecureEnclave.P256.Signing.PrivateKey(accessControl: access)
            return NewDeviceKey(kind: .secureEnclave, data: key.dataRepresentation, signer: SecureEnclaveSigner(key: key))
        }
        guard allowSoftware else { throw DeviceError.noSecureEnclave }
        let key = P256.Signing.PrivateKey()
        return NewDeviceKey(kind: .software, data: key.rawRepresentation, signer: SoftwareSigner(key: key))
    }

    public static func software(_ key: P256.Signing.PrivateKey = .init()) -> NewDeviceKey {
        NewDeviceKey(kind: .software, data: key.rawRepresentation, signer: SoftwareSigner(key: key))
    }
}

public enum DeviceError: Error, Equatable {
    case noSecureEnclave
    case registrationRefused(Int)
    case alreadyRegistered
    case keychain(OSStatus)
}

/// Where the credential is kept.
public protocol CredentialStore: Sendable {
    func load() throws -> DeviceCredential?
    func save(_ credential: DeviceCredential) throws
    func delete() throws
}

/// The keychain: one item, this device only, readable after the first unlock (so a background
/// refresh can sign), never synced or backed up to another phone.
public struct KeychainCredentialStore: CredentialStore {
    let service: String
    let account = "device"

    public init(service: String = "mainloop.device-credential") { self.service = service }

    private var query: [String: Any] {
        [kSecClass as String: kSecClassGenericPassword, kSecAttrService as String: service,
         kSecAttrAccount as String: account, kSecUseDataProtectionKeychain as String: true]
    }

    public func load() throws -> DeviceCredential? {
        var q = query
        q[kSecReturnData as String] = true
        q[kSecMatchLimit as String] = kSecMatchLimitOne
        var out: CFTypeRef?
        let status = SecItemCopyMatching(q as CFDictionary, &out)
        if status == errSecItemNotFound { return nil }
        guard status == errSecSuccess, let data = out as? Data else { throw DeviceError.keychain(status) }
        return try JSONDecoder().decode(DeviceCredential.self, from: data)
    }

    public func save(_ credential: DeviceCredential) throws {
        try delete()
        var q = query
        q[kSecValueData as String] = try JSONEncoder().encode(credential)
        q[kSecAttrAccessible as String] = kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly
        let status = SecItemAdd(q as CFDictionary, nil)
        guard status == errSecSuccess else { throw DeviceError.keychain(status) }
    }

    public func delete() throws {
        let status = SecItemDelete(query as CFDictionary)
        guard status == errSecSuccess || status == errSecItemNotFound else { throw DeviceError.keychain(status) }
    }
}

/// In memory, for tests and previews.
public final class MemoryCredentialStore: CredentialStore, @unchecked Sendable {
    private let lock = NSLock()
    private var value: DeviceCredential?

    public init(_ value: DeviceCredential? = nil) { self.value = value }

    public func load() throws -> DeviceCredential? { lock.withLock { value } }
    public func save(_ credential: DeviceCredential) throws { lock.withLock { value = credential } }
    public func delete() throws { lock.withLock { value = nil } }
}

/// Registers a new key with the server, with the owner's fresh Pocket ID token, whose nonce is
/// the key's thumbprint (`RequestSigner.thumbprint`).
public enum DeviceEnrollment {
    public static func register(
        origin: URL,
        idToken: String,
        name: String,
        key: NewDeviceKey,
        transport: (any ClientTransport)? = nil
    ) async throws -> DeviceCredential {
        let middlewares: [any ClientMiddleware] = [BearerMiddleware(token: idToken)]
        let client = if let transport {
            MainloopAPI.client(origin: origin, middlewares: middlewares, transport: transport)
        } else {
            MainloopAPI.client(origin: origin, middlewares: middlewares)
        }
        let jwk = key.signer.publicJWK
        let body = Components.Schemas.DeviceRegistration(
            name: name, platform: .ios, requestKey: .init(kty: .ec, crv: .p256, x: jwk.x, y: jwk.y)
        )
        switch try await client.registerDevice(body: .json(body)) {
        case .created(let created):
            let device = try created.body.json
            return DeviceCredential(deviceID: device.id, origin: origin, keyKind: key.kind, keyData: key.data)
        case .conflict: throw DeviceError.alreadyRegistered
        case .unauthorized: throw DeviceError.registrationRefused(401)
        case .forbidden: throw DeviceError.registrationRefused(403)
        case .unprocessableContent: throw DeviceError.registrationRefused(422)
        case .undocumented(let status, _): throw DeviceError.registrationRefused(status)
        }
    }

    /// The API client that signs every request with the device's key.
    public static func client(for credential: DeviceCredential, transport: (any ClientTransport)? = nil) throws -> Client {
        let signing = DeviceSigningMiddleware(deviceID: credential.deviceID, origin: credential.origin, signer: try credential.signer())
        if let transport {
            return MainloopAPI.client(origin: credential.origin, middlewares: [signing], transport: transport)
        }
        return MainloopAPI.client(origin: credential.origin, middlewares: [signing])
    }

    /// Revokes a device the phone could not keep (its keychain refused the credential), with the key
    /// still in memory. Best effort.
    public static func revoke(_ credential: DeviceCredential, transport: (any ClientTransport)? = nil) async {
        if let client = try? client(for: credential, transport: transport) {
            _ = try? await client.revokeDevice(path: .init(deviceId: credential.deviceID))
        }
    }

    /// Signing out: revoke on the server (best effort; a phone offline still forgets its key),
    /// then forget the credential.
    public static func signOut(_ credential: DeviceCredential, store: any CredentialStore, transport: (any ClientTransport)? = nil) async throws {
        if let client = try? client(for: credential, transport: transport) {
            _ = try? await client.revokeDevice(path: .init(deviceId: credential.deviceID))
        }
        try store.delete()
    }
}

/// The phone's registration, if any, shared by the API client and the Settings screen. Loaded
/// from the store once; sign-in and sign-out update both.
public final class DeviceAccount: @unchecked Sendable {
    private let lock = NSLock()
    private let store: any CredentialStore
    private var cached: (credential: DeviceCredential, signer: any RequestSigner)?

    public init(store: any CredentialStore) {
        self.store = store
        if let credential = try? store.load(), let signer = try? credential.signer() {
            cached = (credential, signer)
        }
    }

    public var credential: DeviceCredential? { lock.withLock { cached?.credential } }

    func signing() -> (credential: DeviceCredential, signer: any RequestSigner)? { lock.withLock { cached } }

    public func save(_ credential: DeviceCredential) throws {
        let signer = try credential.signer()
        try store.save(credential)
        lock.withLock { cached = (credential, signer) }
    }

    /// Revokes this device on the server (best effort), then forgets it.
    public func signOut(transport: (any ClientTransport)? = nil) async throws {
        guard let credential else { return }
        lock.withLock { cached = nil }
        try await DeviceEnrollment.signOut(credential, store: store, transport: transport)
    }
}

/// Signs each request with the account's key when the phone is registered with this origin;
/// sends it unsigned otherwise (the server answers 401, and the app offers sign-in).
public struct AccountSigningMiddleware: ClientMiddleware {
    let account: DeviceAccount
    let origin: URL

    public init(account: DeviceAccount, origin: URL) {
        self.account = account
        self.origin = origin
    }

    public func intercept(
        _ request: HTTPRequest,
        body: HTTPBody?,
        baseURL: URL,
        operationID: String,
        next: @Sendable (HTTPRequest, HTTPBody?, URL) async throws -> (HTTPResponse, HTTPBody?)
    ) async throws -> (HTTPResponse, HTTPBody?) {
        // A key registered with one origin never signs for another.
        guard let (credential, signer) = account.signing(), credential.origin == origin else {
            return try await next(request, body, baseURL)
        }
        return try await DeviceSigningMiddleware(deviceID: credential.deviceID, origin: credential.origin, signer: signer)
            .intercept(request, body: body, baseURL: baseURL, operationID: operationID, next: next)
    }
}
