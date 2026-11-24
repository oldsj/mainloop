import Foundation
import MainloopAPI
@testable import MainloopCore
import Testing

@Suite struct ServerOriginTests {
    @Test(arguments: [
        ("https://mainloop.example.org", "https://mainloop.example.org"),
        ("https://Mainloop.Example.org/", "https://mainloop.example.org"),
        ("https://mainloop.example.org:8443", "https://mainloop.example.org:8443"),
        ("http://localhost:5173", "http://localhost:5173"),
        ("http://127.0.0.1:8000", "http://127.0.0.1:8000"),
    ])
    func acceptsOneHTTPSOrigin(_ raw: String, _ expected: String) {
        #expect(ServerOrigin.parse(raw) == .live(URL(string: expected)!))
    }

    @Test(arguments: [
        nil, "", "   ", "mainloop.example.org", "http://mainloop.example.org", "ftp://mainloop.example.org",
        "https://mainloop.example.test", "https://example.test", "https://mainloop.example",
        "https://mainloop.example.org/app", "https://user:pw@mainloop.example.org",
        "https://mainloop.example.org/?x=1",
    ] as [String?])
    func anythingElseMeansFixtures(_ raw: String?) {
        #expect(ServerOrigin.parse(raw) == .fixtures)
    }

    @Test func apiLivesUnderSlashAPI() {
        let url = MainloopAPI.apiBaseURL(origin: URL(string: "https://mainloop.example.org")!)
        #expect(url.absoluteString == "https://mainloop.example.org/api")
    }
}

@Suite struct EdgeMarginsTests {
    @Test func unionTakesTheLargestPerEdge() {
        let outerCamera = EdgeMargins(top: 0, leading: 0, bottom: 0, trailing: 38)
        let innerCamera = EdgeMargins(top: 52, leading: 0, bottom: 0, trailing: 0)
        #expect(EdgeMargins.union([outerCamera, innerCamera]) == EdgeMargins(top: 52, trailing: 38))
        #expect(EdgeMargins.union([]) == .zero)
    }

    @Test func subtractingTheSafeAreaNeverGoesNegative() {
        let region = EdgeMargins(top: 52, trailing: 38)
        let safe = EdgeMargins(top: 59, leading: 0, bottom: 34, trailing: 20)
        #expect(region.subtracting(safe) == EdgeMargins(trailing: 18))
    }
}
