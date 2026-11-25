import CryptoKit
import Foundation
import MainloopAPI
import HTTPTypes
import OpenAPIRuntime
import Security

/// What the phone keeps after registering (docs/specs/devices.md): its device id, the origin it
/// registered with, its request key and its approval key. On a phone the keys are Secure Enclave
/// blobs that only this phone can use; in the simulator, which has no Secure Enclave, software
/// keys. Both keys are of the same kind.
public struct DeviceCredential: Codable, Equatable, Sendable {
    public enum KeyKind: String, Codable, Sendable { case secureEnclave, software }

    public let deviceID: String
    public let origin: URL
    public let keyKind: KeyKind
    public let keyData: Data
    /// The approval key, which signs approvals of privileged actions behind Touch ID. Nil for a
    /// phone registered before approval keys, or one that couldn't make one (no passcode or no
    /// enrolled biometry): it signs in again to get one.
    public let approvalKeyData: Data?

    public init(deviceID: String, origin: URL, keyKind: KeyKind, keyData: Data, approvalKeyData: Data? = nil) {
        self.deviceID = deviceID
        self.origin = origin
        self.keyKind = keyKind
        self.keyData = keyData
        self.approvalKeyData = approvalKeyData
    }

    public func signer() throws -> any RequestSigner {
        switch keyKind {
        case .secureEnclave: try SecureEnclaveSigner(key: .init(dataRepresentation: keyData))
        case .software: try SoftwareSigner(key: .init(rawRepresentation: keyData))
        }
    }

    /// The approval key's signer; nil without an approval key.
    public func approvalSigner() throws -> (any ApprovalSigner)? {
        guard let approvalKeyData else { return nil }
        return switch keyKind {
        case .secureEnclave: try SecureEnclaveApprovalSigner(keyData: approvalKeyData)
        case .software: try SoftwareSigner(key: .init(rawRepresentation: approvalKeyData))
        }
    }
}

/// New keys, before they are registered: the request key and the approval key.
public struct NewDeviceKey: Sendable {
    public let kind: DeviceCredential.KeyKind
    public let data: Data
    public let signer: any RequestSigner
    public let approvalData: Data?
    public let approvalSigner: (any ApprovalSigner)?

    /// Secure Enclave keys, on this phone only. The request key is usable after the phone's first
    /// unlock and asks for nothing. The approval key needs a passcode set and asks for the current
    /// Touch ID or Face ID enrollment at every signature; without a passcode or enrolled biometry
    /// the phone registers without one. Where there is no Secure Enclave, software keys only if
    /// `allowSoftware` (the simulator).
    public static func make(allowSoftware: Bool) throws -> NewDeviceKey {
        if SecureEnclave.isAvailable {
            let key = try SecureEnclave.P256.Signing.PrivateKey(
                accessControl: accessControl(kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly, .privateKeyUsage)
            )
            let approval = try? SecureEnclave.P256.Signing.PrivateKey(
                accessControl: accessControl(kSecAttrAccessibleWhenPasscodeSetThisDeviceOnly, [.privateKeyUsage, .biometryCurrentSet])
            )
            let approvalSigner = approval.flatMap { try? SecureEnclaveApprovalSigner(keyData: $0.dataRepresentation) }
            return NewDeviceKey(
                kind: .secureEnclave, data: key.dataRepresentation, signer: SecureEnclaveSigner(key: key),
                approvalData: approvalSigner?.keyData, approvalSigner: approvalSigner
            )
        }
        guard allowSoftware else { throw DeviceError.noSecureEnclave }
        return software()
    }

    public static func software(_ key: P256.Signing.PrivateKey = .init(), approval: P256.Signing.PrivateKey? = .init()) -> NewDeviceKey {
        NewDeviceKey(
            kind: .software, data: key.rawRepresentation, signer: SoftwareSigner(key: key),
            approvalData: approval?.rawRepresentation, approvalSigner: approval.map { SoftwareSigner(key: $0) }
        )
    }

    private static func accessControl(_ protection: CFString, _ flags: SecAccessControlCreateFlags) throws -> SecAccessControl {
        var error: Unmanaged<CFError>?
        guard let access = SecAccessControlCreateWithFlags(nil, protection, flags, &error) else {
            throw error!.takeRetainedValue() as Error
        }
        return access
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
        let approvalJWK = key.approvalSigner?.publicJWK
        let body = Components.Schemas.DeviceRegistration(
            name: name, platform: .ios, requestKey: .init(kty: .ec, crv: .p256, x: jwk.x, y: jwk.y),
            approvalKey: approvalJWK.map { .init(kty: .ec, crv: .p256, x: $0.x, y: $0.y) }
        )
        switch try await client.registerDevice(body: .json(body)) {
        case .created(let created):
            let device = try created.body.json
            return DeviceCredential(
                deviceID: device.id, origin: origin, keyKind: key.kind, keyData: key.data, approvalKeyData: key.approvalData
            )
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

    /// Signing out: revoke on the server (best effort; a phone offline still forgets its keys),
    /// then forget the credential, with both keys.
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
    private var cached: Cached?

    private struct Cached {
        let credential: DeviceCredential
        let signer: any RequestSigner
        let approvalSigner: (any ApprovalSigner)?
    }

    public init(store: any CredentialStore) {
        self.store = store
        if let credential = try? store.load(), let signer = try? credential.signer() {
            // An approval key the phone can no longer use counts as none: sign in again.
            cached = Cached(credential: credential, signer: signer, approvalSigner: try? credential.approvalSigner())
        }
    }

    public var credential: DeviceCredential? { lock.withLock { cached?.credential } }

    func signing() -> (credential: DeviceCredential, signer: any RequestSigner)? {
        lock.withLock { cached.map { ($0.credential, $0.signer) } }
    }

    /// The registration and its approval key, if it has one.
    func approving() -> (credential: DeviceCredential, signer: any ApprovalSigner)? {
        lock.withLock {
            guard let cached, let signer = cached.approvalSigner else { return nil }
            return (cached.credential, signer)
        }
    }

    public func save(_ credential: DeviceCredential) throws {
        let signer = try credential.signer()
        let approvalSigner = try credential.approvalSigner()
        try store.save(credential)
        lock.withLock { cached = Cached(credential: credential, signer: signer, approvalSigner: approvalSigner) }
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

/// Signs approvals of privileged actions for the approval sheet (docs/specs/ios-app.md).
public protocol Approver: Sendable {
    /// Whether this phone can sign approvals: registered with an approval key.
    var canApprove: Bool { get }
    /// An approval token for the action (`ApprovalToken`). Signing asks for Touch ID on a phone.
    func approval(actionID: String, digest: String, reason: String) async throws -> String
}

/// Signs with the account's approval key, for the origin it registered with only.
public struct DeviceApprover: Approver {
    let account: DeviceAccount
    let origin: URL

    public init(account: DeviceAccount, origin: URL) {
        self.account = account
        self.origin = origin
    }

    private var approving: (credential: DeviceCredential, signer: any ApprovalSigner)? {
        guard let approving = account.approving(), approving.credential.origin == origin else { return nil }
        return approving
    }

    public var canApprove: Bool { approving != nil }

    public func approval(actionID: String, digest: String, reason: String) async throws -> String {
        guard let (credential, signer) = approving else { throw ApprovalError.noApprovalKey }
        return try await ApprovalToken.make(
            deviceID: credential.deviceID, audience: DeviceToken.audience(for: credential.origin),
            actionID: actionID, digest: digest, signer: signer, reason: reason
        )
    }
}

public enum ApprovalError: Error, Equatable, Sendable {
    /// Registered before approval keys: sign out and in again.
    case noApprovalKey
}
