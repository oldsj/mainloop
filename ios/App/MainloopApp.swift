import MainloopAPI
import MainloopCore
import SwiftUI

@main
struct MainloopApp: App {
    @State private var environment = AppEnvironment.fromBundle()

    var body: some Scene {
        WindowGroup {
            RootView()
                .environment(environment)
                .environment(environment.needsYou)
                .environment(environment.sessions)
        }
    }
}

/// What the app runs against, decided once at launch from the `MainloopOrigin` build setting.
@MainActor @Observable
final class AppEnvironment {
    let origin: ServerOrigin
    let needsYou: NeedsYouModel
    let sessions: SessionsModel

    init(origin: ServerOrigin, source: any MainloopSource) {
        self.origin = origin
        needsYou = NeedsYouModel(source: source)
        sessions = SessionsModel(source: source)
    }

    static func fromBundle(_ bundle: Bundle = .main) -> AppEnvironment {
        let origin = ServerOrigin.parse(bundle.object(forInfoDictionaryKey: ServerOrigin.infoPlistKey) as? String)
        switch origin {
        case .live(let url):
            return AppEnvironment(origin: origin, source: LiveSource(client: MainloopAPI.client(origin: url)))
        case .fixtures:
            // Fixtures are bundled with MainloopCore; failing to read them is a build error.
            return AppEnvironment(origin: origin, source: try! FixtureSource())
        }
    }

    var isFixtures: Bool { origin == .fixtures }

    var webOrigin: URL? {
        if case .live(let url) = origin { url } else { nil }
    }
}
