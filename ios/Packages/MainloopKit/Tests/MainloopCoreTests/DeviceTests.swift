import CryptoKit
import Foundation
import HTTPTypes
import MainloopAPI
@testable import MainloopCore
import OpenAPIRuntime
import Testing

/// Records what the client sends and answers with a canned response.
actor FakeTransport: ClientTransport {
    struct Sent: Sendable {
        let request: HTTPRequest
        let body: Data?
        let baseURL: URL
    }

    private(set) var sent: [Sent] = []
    let status: HTTPResponse.Status
    let json: String?
    let fails: Bool
    /// Answers for particular operations, by operation id, with 200.
    let routes: [String: String]

    init(status: HTTPResponse.Status = .ok, json: String? = nil, fails: Bool = false, routes: [String: String] = [:]) {
        self.status = status
        self.json = json
        self.fails = fails
        self.routes = routes
    }

    func send(_ request: HTTPRequest, body: HTTPBody?, baseURL: URL, operationID: String) async throws -> (HTTPResponse, HTTPBody?) {
        let data: Data? = if let body { try await Data(collecting: body, upTo: 1 << 20) } else { nil }
        sent.append(Sent(request: request, body: data, baseURL: baseURL))
        if fails { throw URLError(.notConnectedToInternet) }
        if let routed = routes[operationID] {
            var response = HTTPResponse(status: .ok)
            response.headerFields[.contentType] = "application/json"
            return (response, HTTPBody(routed))
        }
        var response = HTTPResponse(status: status)
        guard let json else { return (response, nil) }
        response.headerFields[.contentType] = "application/json"
        return (response, HTTPBody(json))
    }
}

/// A decoded device token, checked against the signer's public key.
struct DecodedToken {
    let header: [String: Any]
    let claims: [String: Any]

    init(_ token: String, key: P256.Signing.PublicKey, verify: Bool = true) throws {
        let parts = token.split(separator: ".").map(String.init)
        try #require(parts.count == 3)
        let signature = try P256.Signing.ECDSASignature(rawRepresentation: try #require(Base64URL.decode(parts[2])))
        try #require(!verify || key.isValidSignature(signature, for: Data((parts[0] + "." + parts[1]).utf8)))
        header = try JSONSerialization.jsonObject(with: try #require(Base64URL.decode(parts[0]))) as! [String: Any]
        claims = try JSONSerialization.jsonObject(with: try #require(Base64URL.decode(parts[1]))) as! [String: Any]
    }
}

/// Whether the token's signature verifies with `key`.
func verifies(_ token: String, key: P256.Signing.PublicKey) -> Bool {
    let parts = token.split(separator: ".").map(String.init)
    guard parts.count == 3, let raw = Base64URL.decode(parts[2]),
          let signature = try? P256.Signing.ECDSASignature(rawRepresentation: raw) else { return false }
    return key.isValidSignature(signature, for: Data((parts[0] + "." + parts[1]).utf8))
}

let deviceID = "dev_0123456789abcdefghijklmnop"
let origin = URL(string: "https://mainloop.example.test")!

func bodyHash(_ data: Data) -> String { Base64URL.encode(Data(SHA256.hash(data: data))) }

func bearer(_ sent: FakeTransport.Sent) throws -> String {
    let value = try #require(sent.request.headerFields[.authorization])
    try #require(value.hasPrefix("Bearer "))
    return String(value.dropFirst("Bearer ".count))
}

@Suite struct DeviceTokenTests {
    @Test func aTokenCarriesTheClaimsTheServerChecks() throws {
        let signer = SoftwareSigner()
        let now = Date(timeIntervalSince1970: 1_790_000_000)
        let token = try DeviceToken.make(
            deviceID: deviceID, audience: "https://mainloop.example.test", method: "POST", target: "/queue/q-1/respond?x=1",
            body: Data("{}".utf8), signer: signer, now: now, jti: "0123456789abcdef0123"
        )
        let decoded = try DecodedToken(token, key: signer.key.publicKey)
        #expect(decoded.header["alg"] as? String == "ES256")
        #expect(decoded.header["typ"] as? String == "mainloop-device+jwt")
        #expect(decoded.header["kid"] as? String == deviceID)
        #expect(decoded.claims["iss"] as? String == "mainloop-device")
        #expect(decoded.claims["sub"] as? String == deviceID)
        #expect(decoded.claims["aud"] as? String == "https://mainloop.example.test")
        #expect(decoded.claims["iat"] as? Int == 1_790_000_000)
        #expect(decoded.claims["exp"] as? Int == 1_790_000_060)
        #expect(decoded.claims["jti"] as? String == "0123456789abcdef0123")
        #expect(decoded.claims["htm"] as? String == "POST")
        #expect(decoded.claims["htu"] as? String == "/queue/q-1/respond?x=1")
        #expect(decoded.claims["bh"] as? String == bodyHash(Data("{}".utf8)))
    }

    @Test func theAudienceIsTheInstancesOrigin() {
        #expect(DeviceToken.audience(for: URL(string: "https://Mainloop.Example.TEST/")!) == "https://mainloop.example.test")
        #expect(DeviceToken.audience(for: URL(string: "http://localhost:5173/api")!) == "http://localhost:5173")
    }

    @Test func theThumbprintIsRFC7638() throws {
        // RFC 7638's own example is RSA; this is the same construction over a P-256 key, with the
        // value computed independently (Python: sha256 of the canonical JSON, base64url).
        let x = "f83OJ3D2xF1Bg8vub9tLe1gHMzV76e8Tus9uPHvRVEU", y = "x_FEzRu9m36HLN_tue659LNpXW6pCyStikYjKIWI5a0"
        struct Fixed: RequestSigner {
            let publicJWK: (x: String, y: String)
            func sign(_: Data) throws -> Data { Data() }
        }
        #expect(Fixed(publicJWK: (x, y)).thumbprint == "oKIywvGUpTVTyxMQ3bwIIeQUudfr_CkLMjCE19ECD-U")
    }

    @Test func everyTokenGetsItsOwnID() {
        let ids = Set((0 ..< 50).map { _ in DeviceToken.randomID() })
        #expect(ids.count == 50)
        #expect(ids.allSatisfy { $0.count == 32 })
    }

    @Test func theJWKIsTheRawPublicKey() throws {
        let signer = SoftwareSigner()
        let jwk = signer.publicJWK
        #expect(jwk.x.count == 43 && jwk.y.count == 43)
        let raw = try #require(Base64URL.decode(jwk.x)) + #require(Base64URL.decode(jwk.y))
        #expect(raw == signer.key.publicKey.rawRepresentation)
    }
}

/// A fictional action id and digest (sha256 of "fixture").
let actionID = "pa_fixture0001"
let digest = "f16d05ec6b29248d2c61adb1e9263f78e4f7bace1b955014a2d17872cfe4064d"

@Suite struct ApprovalTokenTests {
    @Test func anApprovalCarriesTheActionAndDigest() async throws {
        let signer = SoftwareSigner()
        let token = try await ApprovalToken.make(
            deviceID: deviceID, audience: DeviceToken.audience(for: origin), actionID: actionID, digest: digest,
            signer: signer, reason: "Approve: a test", now: Date(timeIntervalSince1970: 1_790_000_000), jti: "approval-jti-0001"
        )
        let decoded = try DecodedToken(token, key: signer.key.publicKey)
        #expect(decoded.header as? [String: String] == ["alg": "ES256", "typ": "mainloop-approval+jwt", "kid": deviceID])
        #expect(Set(decoded.claims.keys) == ["iss", "sub", "aud", "iat", "exp", "jti", "act", "dig"])
        #expect(decoded.claims["iss"] as? String == "mainloop-device")
        #expect(decoded.claims["sub"] as? String == deviceID)
        #expect(decoded.claims["aud"] as? String == "https://mainloop.example.test")
        #expect(decoded.claims["iat"] as? Int == 1_790_000_000)
        #expect(decoded.claims["exp"] as? Int == 1_790_000_120)
        #expect(decoded.claims["jti"] as? String == "approval-jti-0001")
        #expect(decoded.claims["act"] as? String == actionID)
        #expect(decoded.claims["dig"] as? String == digest)
    }

    @Test func anApprovalSignedWithAnotherKeyDoesNotVerify() async throws {
        let token = try await ApprovalToken.make(
            deviceID: deviceID, audience: "https://mainloop.example.test", actionID: actionID, digest: digest,
            signer: SoftwareSigner(), reason: "Approve"
        )
        #expect(!verifies(token, key: SoftwareSigner().key.publicKey))
        let jti = try DecodedToken(token, key: P256.Signing.PrivateKey().publicKey, verify: false).claims["jti"] as? String
        #expect(jti?.count == 32)
    }
}

@Suite struct DeviceApproverTests {
    @Test func aPhoneWithoutAnApprovalKeyCantApprove() async throws {
        let key = NewDeviceKey.software(approval: nil)
        let account = DeviceAccount(store: MemoryCredentialStore(DeviceCredential(deviceID: deviceID, origin: origin, keyKind: .software, keyData: key.data)))
        let approver = DeviceApprover(account: account, origin: origin)
        #expect(!approver.canApprove)
        await #expect(throws: ApprovalError.noApprovalKey) {
            try await approver.approval(actionID: actionID, digest: digest, reason: "Approve")
        }
    }

    @Test func anApprovalIsSignedWithTheApprovalKeyNotTheRequestKey() async throws {
        let request = P256.Signing.PrivateKey(), approval = P256.Signing.PrivateKey()
        let key = NewDeviceKey.software(request, approval: approval)
        let credential = DeviceCredential(deviceID: deviceID, origin: origin, keyKind: .software, keyData: key.data, approvalKeyData: key.approvalData)
        let approver = DeviceApprover(account: DeviceAccount(store: MemoryCredentialStore(credential)), origin: origin)
        #expect(approver.canApprove)
        let token = try await approver.approval(actionID: actionID, digest: digest, reason: "Approve")
        let decoded = try DecodedToken(token, key: approval.publicKey)
        #expect(decoded.claims["aud"] as? String == "https://mainloop.example.test")
        #expect(decoded.claims["act"] as? String == actionID)
        #expect(!verifies(token, key: request.publicKey))
    }

    @Test func anApprovalKeyNeverSignsForAnotherOrigin() {
        let key = NewDeviceKey.software()
        let credential = DeviceCredential(deviceID: deviceID, origin: origin, keyKind: .software, keyData: key.data, approvalKeyData: key.approvalData)
        let account = DeviceAccount(store: MemoryCredentialStore(credential))
        #expect(!DeviceApprover(account: account, origin: URL(string: "https://other.example.test")!).canApprove)
    }
}

@Suite struct DeviceSigningMiddlewareTests {
    @Test func aReadIsSignedForItsPathWithoutTheAPIPrefix() async throws {
        let transport = FakeTransport(json: "[]")
        let signer = SoftwareSigner()
        let credential = DeviceCredential(deviceID: deviceID, origin: origin, keyKind: .software, keyData: signer.key.rawRepresentation)
        let client = try DeviceEnrollment.client(for: credential, transport: transport)

        _ = try await client.listDevices()

        let sent = try #require(await transport.sent.first)
        #expect(sent.baseURL.absoluteString == "https://mainloop.example.test/api")
        #expect(sent.request.headerFields[ClientHeaderMiddleware.name] == "ios")
        let claims = try DecodedToken(try bearer(sent), key: signer.key.publicKey).claims
        #expect(claims["htm"] as? String == "GET")
        #expect(claims["htu"] as? String == "/devices")
        #expect(claims["bh"] as? String == bodyHash(Data()))
    }

    @Test func aWriteIsSignedOverTheBodyItSends() async throws {
        let transport = FakeTransport(json: #"{"status":"ok"}"#)
        let signer = SoftwareSigner()
        let client = MainloopAPI.client(
            origin: origin, middlewares: [DeviceSigningMiddleware(deviceID: deviceID, origin: origin, signer: signer)], transport: transport
        )

        _ = try? await client.respondToQueueItem(path: .init(itemId: "q-0001"), body: .json(.init(response: "Approve")))

        let sent = try #require(await transport.sent.first)
        let body = try #require(sent.body)
        #expect(!body.isEmpty)
        let claims = try DecodedToken(try bearer(sent), key: signer.key.publicKey).claims
        #expect(claims["htm"] as? String == "POST")
        #expect(claims["htu"] as? String == "/queue/q-0001/respond")
        #expect(claims["bh"] as? String == bodyHash(body))
    }

    @Test func twoRequestsNeverShareAToken() async throws {
        let transport = FakeTransport(json: "[]")
        let signer = SoftwareSigner()
        let client = MainloopAPI.client(
            origin: origin, middlewares: [DeviceSigningMiddleware(deviceID: deviceID, origin: origin, signer: signer)], transport: transport
        )
        _ = try await client.listDevices()
        _ = try await client.listDevices()
        let sent = await transport.sent
        let jtis = try sent.map { try DecodedToken(try bearer($0), key: signer.key.publicKey).claims["jti"] as? String }
        #expect(jtis.count == 2 && jtis[0] != jtis[1])
    }
}

@Suite struct DeviceEnrollmentTests {
    let deviceJSON = """
    {"id":"\(deviceID)","name":"Test iPhone","platform":"ios","key_thumbprint":"abc",
     "created_at":"2026-11-24T12:00:00Z","last_seen_at":null,"revoked_at":null,"current":false}
    """

    @Test func registeringSendsThePublicKeyWithThePocketIDToken() async throws {
        let transport = FakeTransport(status: .created, json: deviceJSON)
        let key = NewDeviceKey.software()

        let credential = try await DeviceEnrollment.register(
            origin: origin, idToken: "id-token-fixture", name: "Test iPhone", key: key, transport: transport
        )

        #expect(credential == DeviceCredential(deviceID: deviceID, origin: origin, keyKind: .software, keyData: key.data, approvalKeyData: key.approvalData))
        let sent = try #require(await transport.sent.first)
        #expect(try bearer(sent) == "id-token-fixture")
        #expect(sent.request.method == .post)
        #expect(sent.request.path == "/devices")
        let body = try JSONSerialization.jsonObject(with: try #require(sent.body)) as! [String: Any]
        let requestKey = try #require(body["request_key"] as? [String: String])
        #expect(requestKey == ["kty": "EC", "crv": "P-256", "x": key.signer.publicJWK.x, "y": key.signer.publicJWK.y])
        #expect(body["platform"] as? String == "ios")
        // The stored key signs as the registered one.
        #expect(try credential.signer().publicJWK == key.signer.publicJWK)
    }

    @Test func registeringSendsASecondKeyForApprovals() async throws {
        let transport = FakeTransport(status: .created, json: deviceJSON)
        let key = NewDeviceKey.software()

        let credential = try await DeviceEnrollment.register(
            origin: origin, idToken: "id-token-fixture", name: "Test iPhone", key: key, transport: transport
        )

        let sent = try #require(await transport.sent.first)
        let body = try JSONSerialization.jsonObject(with: try #require(sent.body)) as! [String: Any]
        let requestKey = try #require(body["request_key"] as? [String: String])
        let approvalKey = try #require(body["approval_key"] as? [String: String])
        let approvalJWK = try #require(key.approvalSigner?.publicJWK)
        #expect(approvalKey == ["kty": "EC", "crv": "P-256", "x": approvalJWK.x, "y": approvalJWK.y])
        #expect(approvalKey != requestKey)
        // Kept with the request key, and signs as the registered one.
        #expect(credential.approvalKeyData == key.approvalData)
        let stored = try #require(try credential.approvalSigner())
        #expect(stored.publicJWK == approvalJWK)
    }

    @Test func aPhoneWithoutAnApprovalKeyRegistersWithoutOne() async throws {
        let transport = FakeTransport(status: .created, json: deviceJSON)
        let credential = try await DeviceEnrollment.register(
            origin: origin, idToken: "t", name: "n", key: .software(approval: nil), transport: transport
        )
        let body = try JSONSerialization.jsonObject(with: try #require(await transport.sent.first?.body)) as! [String: Any]
        #expect(body["approval_key"] == nil)
        #expect(credential.approvalKeyData == nil)
        #expect(try credential.approvalSigner() == nil)
    }

    @Test func aRefusalIsReported() async throws {
        let refused = FakeTransport(status: .forbidden, json: #"{"detail":"Not permitted"}"#)
        await #expect(throws: DeviceError.registrationRefused(403)) {
            try await DeviceEnrollment.register(origin: origin, idToken: "t", name: "n", key: .software(), transport: refused)
        }
        let duplicate = FakeTransport(status: .conflict, json: #"{"detail":"Conflict"}"#)
        await #expect(throws: DeviceError.alreadyRegistered) {
            try await DeviceEnrollment.register(origin: origin, idToken: "t", name: "n", key: .software(), transport: duplicate)
        }
    }

    @Test func signingOutRevokesItselfThenForgetsTheKey() async throws {
        let key = NewDeviceKey.software()
        let credential = DeviceCredential(deviceID: deviceID, origin: origin, keyKind: .software, keyData: key.data)
        let store = MemoryCredentialStore(credential)
        let transport = FakeTransport(status: .noContent)

        try await DeviceEnrollment.signOut(credential, store: store, transport: transport)

        let sent = try #require(await transport.sent.first)
        #expect(sent.request.method == .delete)
        #expect(sent.request.path == "/devices/\(deviceID)")
        #expect(try store.load() == nil)
    }

    @Test func signingOutOfflineStillForgetsTheKey() async throws {
        let credential = DeviceCredential(deviceID: deviceID, origin: origin, keyKind: .software, keyData: NewDeviceKey.software().data)
        let store = MemoryCredentialStore(credential)
        try await DeviceEnrollment.signOut(credential, store: store, transport: FakeTransport(fails: true))
        #expect(try store.load() == nil)
    }

    @Test func aCredentialSurvivesEncoding() throws {
        let key = NewDeviceKey.software()
        let credential = DeviceCredential(deviceID: deviceID, origin: origin, keyKind: .software, keyData: key.data, approvalKeyData: key.approvalData)
        let decoded = try JSONDecoder().decode(DeviceCredential.self, from: JSONEncoder().encode(credential))
        #expect(decoded == credential)
        #expect(decoded.approvalKeyData != nil)
    }

    @Test func aCredentialSavedBeforeApprovalKeysHasNone() throws {
        // As the keychain item was written before approval keys existed.
        let keyData = NewDeviceKey.software().data.base64EncodedString()
        let old = #"{"deviceID":"\#(deviceID)","origin":"https:\/\/mainloop.example.test","keyKind":"software","keyData":"\#(keyData)"}"#
        let decoded = try JSONDecoder().decode(DeviceCredential.self, from: Data(old.utf8))
        #expect(decoded.deviceID == deviceID)
        #expect(decoded.approvalKeyData == nil)
        #expect(try decoded.approvalSigner() == nil)
        #expect(!DeviceApprover(account: DeviceAccount(store: MemoryCredentialStore(decoded)), origin: origin).canApprove)
    }
}

@Suite struct PocketIDSignInTests {
    let signIn = PocketIDSignIn(issuer: URL(string: "https://id.example.test")!, clientID: "mainloop-ios")
    let endpoints = PocketIDSignIn.Endpoints(
        issuer: "https://id.example.test",
        authorizationEndpoint: URL(string: "https://id.example.test/authorize")!,
        tokenEndpoint: URL(string: "https://id.example.test/api/oidc/token")!
    )
    let attempt = PocketIDSignIn.Attempt(verifier: "dBjftJeZ4CVP-mJ92ZbV0ffvzbt6LsgyqWQo6sYOvEU", state: "state-1", nonce: "nonce-1")

    // The expected value is computed outside Swift: base64url(sha256(verifier)), unpadded.
    @Test func theChallengeIsTheVerifiersSHA256() {
        #expect(PKCE.challenge(for: "dBjftJeZ4CVP-mJ92ZbV0ffvzbt6LsgyqWQo6sYOvEU") == "qaXBbiZDbF7ISLEFHyh4ZJk90Pu736q4qkxvxzh-IA0")
        #expect(PKCE.verifier().count == 43)
    }

    @Test func theAuthorizationRequestAsksForAFreshSignInWithPKCE() throws {
        let url = signIn.authorizationURL(endpoints, attempt)
        let items = try #require(URLComponents(url: url, resolvingAgainstBaseURL: false)?.queryItems)
        let query = Dictionary(uniqueKeysWithValues: items.map { ($0.name, $0.value ?? "") })
        #expect(query == [
            "response_type": "code", "client_id": "mainloop-ios", "redirect_uri": "mainloop-ios://oauth/callback",
            "scope": "openid profile email groups", "state": "state-1", "nonce": "nonce-1",
            "code_challenge": "qaXBbiZDbF7ISLEFHyh4ZJk90Pu736q4qkxvxzh-IA0", "code_challenge_method": "S256",
            "prompt": "login", "max_age": "600",
        ])
        #expect(url.host() == "id.example.test")
    }

    @Test func theCallbackMustBelongToThisAttempt() throws {
        let ok = URL(string: "mainloop-ios://oauth/callback?code=abc&state=state-1")!
        #expect(try signIn.code(from: ok, attempt) == "abc")
        #expect(throws: PocketIDSignIn.Failure.callback("state does not match")) {
            try signIn.code(from: URL(string: "mainloop-ios://oauth/callback?code=abc&state=other")!, attempt)
        }
        #expect(throws: PocketIDSignIn.Failure.callback("access_denied")) {
            try signIn.code(from: URL(string: "mainloop-ios://oauth/callback?error=access_denied&state=state-1")!, attempt)
        }
        #expect(throws: PocketIDSignIn.Failure.callback("not this app's callback")) {
            try signIn.code(from: URL(string: "https://evil.example.test/?code=abc&state=state-1")!, attempt)
        }
    }

    @Test func theTokenRequestIsAFormWithTheVerifier() {
        let form = String(decoding: PocketIDSignIn.form(["b": "x y", "a": "mainloop-ios://oauth/callback"]), as: UTF8.self)
        #expect(form == "a=mainloop-ios%3A%2F%2Foauth%2Fcallback&b=x%20y")
    }

    @Test func claimsAreReadFromTheTokensPayload() {
        let payload = Base64URL.encode(Data(#"{"nonce":"nonce-1"}"#.utf8))
        #expect(PocketIDSignIn.claim("nonce", in: "e30.\(payload).sig") == "nonce-1")
        #expect(PocketIDSignIn.claim("nonce", in: "garbage") == nil)
    }
}

/// Writes a token this code signed for the Go verifier's interop test
/// (server/internal/auth/testdata/swift-device-token.json). Runs only when asked:
/// `MAINLOOP_WRITE_DEVICE_FIXTURE=<path> swift test --filter writeTheGoInteropFixture`.
/// The Go tests' instance origin (authtest.DeviceOrigin).
let fixtureOrigin = URL(string: "https://mainloop.test")!

@Suite struct InteropFixture {
    @Test func writeTheGoInteropFixture() throws {
        guard let path = ProcessInfo.processInfo.environment["MAINLOOP_WRITE_DEVICE_FIXTURE"] else { return }
        let signer = SoftwareSigner()
        let body = #"{"response":"Approve"}"#
        let iat = 1_790_000_000
        let token = try DeviceToken.make(
            deviceID: deviceID, audience: DeviceToken.audience(for: fixtureOrigin), method: "POST",
            target: "/queue/q-0001/respond?wait=1", body: Data(body.utf8),
            signer: signer, now: Date(timeIntervalSince1970: TimeInterval(iat)), jti: "fixture-jti-0000000001"
        )
        let fixture: [String: Any] = [
            "comment": "Signed by ios/Packages/MainloopKit DeviceToken with a throwaway software key; only the public key is kept.",
            "device_id": deviceID, "x": signer.publicJWK.x, "y": signer.publicJWK.y, "thumbprint": signer.thumbprint,
            "audience": DeviceToken.audience(for: fixtureOrigin),
            "method": "POST", "target": "/queue/q-0001/respond?wait=1", "body": body, "iat": iat, "token": token,
        ]
        let data = try JSONSerialization.data(withJSONObject: fixture, options: [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes])
        try (data + Data("\n".utf8)).write(to: URL(fileURLWithPath: path))
    }

    /// Writes an approval this code signed for the Go approval verifier's test
    /// (server/internal/approval/testdata/swift-approval-token.json). Runs only when asked:
    /// `MAINLOOP_WRITE_APPROVAL_FIXTURE=<path> swift test --filter writeTheGoApprovalFixture`.
    @Test func writeTheGoApprovalFixture() async throws {
        guard let path = ProcessInfo.processInfo.environment["MAINLOOP_WRITE_APPROVAL_FIXTURE"] else { return }
        let signer = SoftwareSigner()
        let iat = 1_790_000_000
        let token = try await ApprovalToken.make(
            deviceID: deviceID, audience: DeviceToken.audience(for: fixtureOrigin), actionID: actionID, digest: digest,
            signer: signer, reason: "Approve: fixture", now: Date(timeIntervalSince1970: TimeInterval(iat)),
            jti: "fixture-approval-jti-0001"
        )
        let fixture: [String: Any] = [
            "comment": "Signed by ios/Packages/MainloopKit ApprovalToken with a throwaway software key; only the public key is kept.",
            "device_id": deviceID, "x": signer.publicJWK.x, "y": signer.publicJWK.y, "thumbprint": signer.thumbprint,
            "audience": DeviceToken.audience(for: fixtureOrigin),
            "action_id": actionID, "digest": digest, "iat": iat, "token": token,
        ]
        let data = try JSONSerialization.data(withJSONObject: fixture, options: [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes])
        try (data + Data("\n".utf8)).write(to: URL(fileURLWithPath: path))
    }
}

@Suite struct DeviceAccountTests {
    func client(_ account: DeviceAccount, _ transport: FakeTransport, origin: URL = origin) -> Client {
        MainloopAPI.client(origin: origin, middlewares: [AccountSigningMiddleware(account: account, origin: origin)], transport: transport)
    }

    @Test func anUnregisteredPhoneSendsNoCredential() async throws {
        let transport = FakeTransport(json: "[]")
        _ = try await client(DeviceAccount(store: MemoryCredentialStore()), transport).listDevices()
        #expect(await transport.sent.first?.request.headerFields[.authorization] == nil)
    }

    @Test func aRegisteredPhoneSignsFromTheNextRequest() async throws {
        let account = DeviceAccount(store: MemoryCredentialStore())
        let transport = FakeTransport(json: "[]")
        let api = client(account, transport)
        let key = NewDeviceKey.software()
        try account.save(DeviceCredential(deviceID: deviceID, origin: origin, keyKind: .software, keyData: key.data))

        _ = try await api.listDevices()

        let sent = try #require(await transport.sent.first)
        let publicKey = try P256.Signing.PublicKey(rawRepresentation: #require(Base64URL.decode(key.signer.publicJWK.x)) + #require(Base64URL.decode(key.signer.publicJWK.y)))
        #expect(try DecodedToken(try bearer(sent), key: publicKey).claims["sub"] as? String == deviceID)
    }

    @Test func aKeyNeverSignsForAnotherOrigin() async throws {
        let store = MemoryCredentialStore(DeviceCredential(deviceID: deviceID, origin: origin, keyKind: .software, keyData: NewDeviceKey.software().data))
        let transport = FakeTransport(json: "[]")
        _ = try await client(DeviceAccount(store: store), transport, origin: URL(string: "https://other.example.test")!).listDevices()
        #expect(await transport.sent.first?.request.headerFields[.authorization] == nil)
    }

    @Test func signingOutStopsSigningAndForgets() async throws {
        let store = MemoryCredentialStore(DeviceCredential(deviceID: deviceID, origin: origin, keyKind: .software, keyData: NewDeviceKey.software().data))
        let account = DeviceAccount(store: store)
        #expect(account.credential?.deviceID == deviceID)
        try await account.signOut(transport: FakeTransport(status: .noContent))
        #expect(account.credential == nil)
        #expect(try store.load() == nil)
    }
}

@Suite struct SignInSettingsTests {
    @Test func signInNeedsAnHTTPSIssuerAndAClient() {
        #expect(PocketIDSignIn.fromSettings(issuer: "https://id.example.test", clientID: "mainloop-ios")?.clientID == "mainloop-ios")
        #expect(PocketIDSignIn.fromSettings(issuer: "", clientID: "mainloop-ios") == nil)
        #expect(PocketIDSignIn.fromSettings(issuer: "http://id.example.test", clientID: "mainloop-ios") == nil)
        #expect(PocketIDSignIn.fromSettings(issuer: "https://id.example.test", clientID: "") == nil)
        #expect(PocketIDSignIn.fromSettings(issuer: nil, clientID: nil) == nil)
        #expect(PocketIDSignIn.fromSettings(issuer: "https:", clientID: "x") == nil)
    }
}
