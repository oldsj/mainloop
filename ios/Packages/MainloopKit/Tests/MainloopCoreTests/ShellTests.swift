import Foundation
import HTTPTypes
import MainloopAPI
@testable import MainloopCore
import Testing

@Suite struct WebSessionTests {
    let token = String(repeating: "aZ0_-", count: 8) + "abc"

    @Test func theCookieIsHostOnlyHttpOnlySecureAndStrict() throws {
        let grant = WebSessionGrant(token: token, expiresAt: Date().addingTimeInterval(3600))
        let cookie = try #require(grant.cookie(for: origin))
        #expect(cookie.name == "__Host-mainloop-device")
        #expect(cookie.value == token)
        #expect(cookie.domain == "mainloop.example.test") // no leading dot: this host only
        #expect(cookie.path == "/")
        #expect(cookie.isSecure)
        #expect(cookie.isHTTPOnly)
        #expect(cookie.sameSitePolicy == .sameSiteStrict)
        #expect(cookie.isSessionOnly)
    }

    @Test func itIsRenewedWithTenMinutesLeft() {
        let now = Date()
        #expect(!WebSessionGrant(token: token, expiresAt: now.addingTimeInterval(3600)).needsRenewal(at: now))
        #expect(WebSessionGrant(token: token, expiresAt: now.addingTimeInterval(9 * 60)).needsRenewal(at: now))
        #expect(WebSessionGrant(token: token, expiresAt: now.addingTimeInterval(-1)).needsRenewal(at: now))
    }

    @Test func startingTakesOnlyTheServersCookieAndAWellFormedToken() async throws {
        func start(_ json: String, status: HTTPResponse.Status = .created) async throws -> WebSessionGrant {
            let client = MainloopAPI.client(origin: origin, middlewares: [], transport: FakeTransport(status: status, json: json))
            return try await WebSessionGrant.start(client: client)
        }
        let ok = #"{"cookie_name":"__Host-mainloop-device","token":"\#(token)","expires_at":"2026-11-25T13:00:00Z"}"#
        #expect(try await start(ok).token == token)
        let other = ok.replacingOccurrences(of: "__Host-mainloop-device", with: "mainloop-device")
        await #expect(throws: WebSessionError.malformed) { try await start(other) }
        let bad = ok.replacingOccurrences(of: token, with: token.dropLast(3) + "a;b")
        await #expect(throws: WebSessionError.malformed) { try await start(bad) }
        await #expect(throws: WebSessionError.notPaired) { try await start(#"{"detail":"x"}"#, status: .unauthorized) }
        await #expect(throws: WebSessionError.notPaired) { try await start(#"{"detail":"x"}"#, status: .forbidden) }
    }
}

@Suite struct ShellMessageTests {
    @Test func approveNamesTheCardAndIgnoresThePagesDigest() {
        let message = ShellMessage(body: ["type": "approve", "item_id": "q-1", "action_id": "pa-1", "digest": String(repeating: "f", count: 64)] as [String: Any])
        #expect(message == .approve(ApprovalRequest(queueItemID: "q-1", actionID: "pa-1")!))
    }

    @Test(arguments: [
        ["type": "approve", "item_id": "q-1"],
        ["type": "approve", "item_id": "", "action_id": "pa-1"],
        ["type": "approve", "item_id": "q 1", "action_id": "pa-1"],
        ["type": "approve", "item_id": "q-1", "action_id": "pa-\u{202E}1"],
        ["type": "approve", "item_id": "q-1", "action_id": 7],
        ["type": "signOut"],
        ["item_id": "q-1"],
    ] as [[String: any Sendable]])
    func anythingElseIsIgnored(_ body: [String: any Sendable]) {
        #expect(ShellMessage(body: body as [String: Any]) == nil)
    }

    @Test func theOtherMessages() {
        #expect(ShellMessage(body: ["type": "sessionExpired"]) == .sessionExpired)
        #expect(ShellMessage(body: ["type": "openDeviceSettings"]) == .openDeviceSettings)
        #expect(ShellMessage(body: "approve") == nil)
    }
}

@Suite struct ShellNavigationTests {
    let mainloop = URL(string: "https://mainloop.example.org")!

    @Test(arguments: [
        ("https://mainloop.example.org/inbox?item=q1", true, ShellNavigation.load),
        ("https://MAINLOOP.example.org:443/tasks/t1", true, .load),
        ("https://mainloop.example.org.evil.test/", true, .openOutside),
        ("https://github.com/octo-org/widgets/pull/7", true, .openOutside),
        ("http://mainloop.example.org/", true, .openOutside),
        ("https://mainloop.example.org:8443/", true, .openOutside),
        ("javascript:alert(1)", true, .refuse),
        ("data:text/html,hi", true, .refuse),
        ("about:blank", true, .refuse),
        ("mainloop-ios://oauth/callback", true, .refuse),
        // Frames inside a page (a workspace preview) load web pages; the bridge never answers them.
        ("https://3000--w1--preview.preview.test/", false, .load),
        ("about:srcdoc", false, .load),
        ("javascript:alert(1)", false, .refuse),
    ])
    func decides(_ raw: String, _ mainFrame: Bool, _ expected: ShellNavigation) throws {
        #expect(ShellNavigation.decide(try #require(URL(string: raw)), origin: mainloop, mainFrame: mainFrame) == expected)
    }

    @Test func aFramesSecurityOriginMustBeMainloops() {
        #expect(ShellNavigation.isOrigin(scheme: "https", host: "mainloop.example.org", port: 0, of: mainloop))
        #expect(ShellNavigation.isOrigin(scheme: "https", host: "mainloop.example.org", port: 443, of: mainloop))
        #expect(!ShellNavigation.isOrigin(scheme: "https", host: "preview.example.org", port: 0, of: mainloop))
        #expect(!ShellNavigation.isOrigin(scheme: "http", host: "mainloop.example.org", port: 0, of: mainloop))
        #expect(!ShellNavigation.isOrigin(scheme: "https", host: "mainloop.example.org", port: 8443, of: mainloop))
    }
}

@Suite struct PushWebPathTests {
    @Test func aTapOpensTheWebPage() {
        #expect(PushRoute.needsYou.webPath == "/inbox")
        #expect(PushRoute.needsYouItem("queue:q-0001").webPath == "/inbox?item=q-0001")
        #expect(PushRoute.needsYouItem("queue:a&b=c").webPath == "/inbox?item=a%26b%3Dc")
        #expect(PushRoute.needsYouItem("problem:x").webPath == "/inbox")
        #expect(PushRoute.needsYouItem("queue:").webPath == "/inbox")
        #expect(PushRoute.webPage("/tasks/t1").webPath == "/tasks/t1")
    }
}

/// The native screens use the web's default theme; every value must be the token's.
@Suite struct PaletteTests {
    static let tokens = URL(fileURLWithPath: #filePath)
        .deletingLastPathComponent().appendingPathComponent("../../../../../web/design/tokens.json").standardizedFileURL

    @Test func everyColourIsTheWebsToken() throws {
        let root = try JSONSerialization.jsonObject(with: Data(contentsOf: Self.tokens)) as! [String: Any]
        let term = ((((root["color"] as! [String: Any])["theme"] as! [String: Any])["eighties"] as! [String: Any])["term"] as! [String: Any])
        for (name, value) in Palette.tokens {
            let token = try #require(term[name] as? [String: Any], "no token \(name)")
            #expect((token["$value"] as? String)?.lowercased() == value, "\(name)")
        }
    }
}
