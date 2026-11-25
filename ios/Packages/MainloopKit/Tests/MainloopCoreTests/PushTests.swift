import CryptoKit
import Foundation
import HTTPTypes
import MainloopAPI
@testable import MainloopCore
import OpenAPIRuntime
import Testing

@Suite struct APNsTokenTests {
    @Test func aTokenIsLowerCaseHex() {
        #expect(APNsToken.hex(Data([0x00, 0x01, 0x7F, 0x80, 0xAB, 0xFF])) == "00017f80abff")
        #expect(APNsToken.hex(Data()) == "")
        let token = Data((0 ..< 32).map { UInt8($0 * 8) })
        let hex = APNsToken.hex(token)
        #expect(hex.count == 64)
        #expect(hex == token.map { String(format: "%02x", $0) }.joined())
    }
}

@Suite struct PushRegistrationTests {
    let token = Data((0 ..< 32).map { UInt8(255 - $0) })
    let deviceJSON = """
    {"id":"\(deviceID)","name":"Test iPhone","platform":"ios","key_thumbprint":"abc",
     "created_at":"2026-11-24T12:00:00Z","last_seen_at":null,"revoked_at":null,"current":true,
     "push":{"environment":"development","updated_at":"2026-11-24T12:01:00Z","invalid_at":null}}
    """

    @Test func theTokenIsPutSignedWithTheDeviceKey() async throws {
        let transport = FakeTransport(json: deviceJSON)
        let signer = SoftwareSigner()
        let credential = DeviceCredential(deviceID: deviceID, origin: origin, keyKind: .software, keyData: signer.key.rawRepresentation)
        let client = try DeviceEnrollment.client(for: credential, transport: transport)

        let push = try await PushRegistration.send(token: token, environment: .development, client: client)

        #expect(push?.environment == .development)
        #expect(push?.invalidAt == nil)
        let sent = try #require(await transport.sent.first)
        #expect(sent.request.method == .put)
        #expect(sent.request.path == "/devices/current/push")
        let body = try #require(sent.body)
        let json = try JSONSerialization.jsonObject(with: body) as! [String: String]
        #expect(json == ["token": APNsToken.hex(token), "environment": "development"])
        let claims = try DecodedToken(try bearer(sent), key: signer.key.publicKey).claims
        #expect(claims["sub"] as? String == deviceID)
        #expect(claims["htm"] as? String == "PUT")
        #expect(claims["htu"] as? String == "/devices/current/push")
        #expect(claims["bh"] as? String == bodyHash(body))
    }

    @Test func theAccountClientSignsItOnceRegistered() async throws {
        let key = NewDeviceKey.software()
        let account = DeviceAccount(store: MemoryCredentialStore(
            DeviceCredential(deviceID: deviceID, origin: origin, keyKind: .software, keyData: key.data)
        ))
        let transport = FakeTransport(json: deviceJSON)
        let client = MainloopAPI.client(origin: origin, middlewares: [AccountSigningMiddleware(account: account, origin: origin)], transport: transport)

        try await PushRegistration.send(token: token, environment: .production, client: client)

        let sent = try #require(await transport.sent.first)
        #expect(sent.request.headerFields[.authorization]?.hasPrefix("Bearer ") == true)
        let json = try JSONSerialization.jsonObject(with: try #require(sent.body)) as! [String: String]
        #expect(json["environment"] == "production")
    }

    @Test func refusalsAreMapped() async throws {
        let cases: [(HTTPResponse.Status, String, PushError)] = [
            (.unauthorized, #"{"detail":"Not authenticated"}"#, .signedOut),
            (.forbidden, #"{"detail":"Only a device may set a push token"}"#, .refused(403)),
            (.notFound, #"{"detail":"Not Found"}"#, .refused(404)),
            (.unprocessableContent, #"{"detail":[]}"#, .refused(422)),
            (.internalServerError, #"{"detail":"boom"}"#, .refused(500)),
        ]
        for (status, json, expected) in cases {
            let client = MainloopAPI.client(origin: origin, middlewares: [], transport: FakeTransport(status: status, json: json))
            await #expect(throws: expected) {
                try await PushRegistration.send(token: token, environment: .development, client: client)
            }
        }
    }

    @Test func anOfflinePhoneGetsTheTransportError() async {
        let client = MainloopAPI.client(origin: origin, middlewares: [], transport: FakeTransport(fails: true))
        await #expect(throws: (any Error).self) {
            try await PushRegistration.send(token: token, environment: .development, client: client)
        }
    }

    @Test func theStatusReportsAnInvalidToken() async throws {
        let json = deviceJSON.replacingOccurrences(of: #""invalid_at":null"#, with: #""invalid_at":"2026-11-24T13:00:00Z""#)
        let transport = FakeTransport(json: json)
        let client = MainloopAPI.client(origin: origin, middlewares: [], transport: transport)

        let push = try await PushRegistration.status(client: client)

        #expect(push?.invalidAt == ISO8601DateFormatter().date(from: "2026-11-24T13:00:00Z"))
        let sent = try #require(await transport.sent.first)
        #expect(sent.request.method == .get)
        #expect(sent.request.path == "/devices/current")
    }

    @Test func aDeviceWithoutATokenHasNoStatus() async throws {
        let json = deviceJSON.replacingOccurrences(
            of: #"{"environment":"development","updated_at":"2026-11-24T12:01:00Z","invalid_at":null}"#, with: "null"
        )
        let client = MainloopAPI.client(origin: origin, middlewares: [], transport: FakeTransport(json: json))
        #expect(try await PushRegistration.status(client: client) == nil)
    }

    @Test func theEnvironmentComesFromTheBuild() {
        #expect(PushRegistration.environment(infoPlistValue: "development") == .development)
        #expect(PushRegistration.environment(infoPlistValue: " Production ") == .production)
        #expect(PushRegistration.environment(infoPlistValue: "") == nil)
        #expect(PushRegistration.environment(infoPlistValue: "$(MAINLOOP_APNS_ENVIRONMENT)") == nil)
        #expect(PushRegistration.environment(infoPlistValue: nil) == nil)
    }
}

@Suite struct PushRouteTests {
    func payload(_ mainloop: Any?, category: String = PushCategory.needsYou) -> [AnyHashable: Any] {
        var info: [AnyHashable: Any] = [
            "aps": ["alert": ["title": "Deploy the docs site?", "body": "Approve or deny"], "sound": "default", "badge": 3,
                    "thread-id": "needs-you", "category": category, "interruption-level": "time-sensitive"],
        ]
        if let mainloop { info["mainloop"] = mainloop }
        return info
    }

    @Test func anAttentionItemOpensItsCard() {
        let route = PushRoute(userInfo: payload(["type": "attention", "item_id": "queue:q-0001", "task_id": "task-0007", "href": "/sessions/sess-0001"]))
        #expect(route == .needsYouItem("queue:q-0001"))
        #expect(PushRoute(userInfo: payload(["type": "attention", "item_id": "hitl:r-0002", "href": "/"])) == .needsYouItem("hitl:r-0002"))
    }

    @Test func aTaskOpensItsWebPage() {
        let task = payload(["type": "task", "task_id": "task-0007", "href": "/tasks/task-0007"], category: PushCategory.task)
        #expect(PushRoute(userInfo: task) == .webPage("/tasks/task-0007"))
        // Without an href, the task's page from its id.
        #expect(PushRoute(userInfo: payload(["type": "task", "task_id": "task-0008"])) == .webPage("/tasks/task-0008"))
        #expect(PushRoute(userInfo: payload(["type": "task", "task_id": "a/b?c"])) == .webPage("/tasks/a%2Fb%3Fc"))
    }

    @Test func anythingElseOpensTheList() {
        let malformed: [Any?] = [
            nil,
            "not an object",
            [:] as [String: Any],
            ["type": "attention"],
            ["type": "attention", "item_id": ""],
            ["type": "attention", "item_id": 42],
            ["type": "attention", "item_id": "queue:q 1"],
            ["type": "attention", "item_id": String(repeating: "x", count: 201)],
            ["type": "attention", "item_id": "queue:q\u{0}1"],
            ["type": "task"],
            ["type": "task", "task_id": ""],
            ["type": "session", "item_id": "queue:q-0001"],
            ["type": 1],
            ["item_id": "queue:q-0001"],
        ]
        for mainloop in malformed {
            #expect(PushRoute(userInfo: payload(mainloop)) == .needsYou)
        }
        #expect(PushRoute(userInfo: [:]) == .needsYou)
    }

    @Test func aTaskHrefMustBeAPathOnTheSameOrigin() {
        for href in ["https://evil.example.test/tasks/t1", "//evil.example.test/tasks/t1", "/\\evil.example.test", "tasks/t1",
                     "javascript:alert(1)", "/tasks/t 1", "/tasks/\u{e9}", ""] {
            // A bad href falls back to the task id, and without one to the list.
            #expect(PushRoute(userInfo: payload(["type": "task", "task_id": "task-0007", "href": href])) == .webPage("/tasks/task-0007"))
            #expect(PushRoute(userInfo: payload(["type": "task", "href": href])) == .needsYou)
        }
    }

    @Test func aWebPageResolvesAgainstTheAppsOrigin() {
        let origin = URL(string: "https://mainloop.example.test")!
        #expect(PushRoute.url(for: "/tasks/task-0007", origin: origin)?.absoluteString == "https://mainloop.example.test/tasks/task-0007")
        #expect(PushRoute.url(for: "/tasks/a%2Fb?tab=log", origin: origin)?.absoluteString == "https://mainloop.example.test/tasks/a%2Fb?tab=log")
        #expect(PushRoute.url(for: "//evil.example.test/x", origin: origin) == nil)
        #expect(PushRoute.url(for: "https://evil.example.test/x", origin: origin) == nil)
    }

    @Test func categoriesMatchTheServer() {
        #expect(PushCategory.needsYou == "NEEDS_YOU")
        #expect(PushCategory.task == "TASK")
    }
}
