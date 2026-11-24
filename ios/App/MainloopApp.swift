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
                .environment(environment.account)
        }
    }
}

/// What the app runs against, decided once at launch from the `MainloopOrigin` build setting.
@MainActor @Observable
final class AppEnvironment {
    let origin: ServerOrigin
    let needsYou: NeedsYouModel
    let sessions: SessionsModel
    let account: AccountModel

    init(origin: ServerOrigin, source: any MainloopSource, account: AccountModel) {
        self.origin = origin
        needsYou = NeedsYouModel(source: source)
        sessions = SessionsModel(source: source)
        self.account = account
    }

    static func fromBundle(_ bundle: Bundle = .main) -> AppEnvironment {
        let origin = ServerOrigin.parse(bundle.object(forInfoDictionaryKey: ServerOrigin.infoPlistKey) as? String)
        let signIn = PocketIDSignIn.fromSettings(
            issuer: bundle.object(forInfoDictionaryKey: "MainloopOIDCIssuer") as? String,
            clientID: bundle.object(forInfoDictionaryKey: "MainloopOIDCClientID") as? String
        )
        let deviceAccount = DeviceAccount(store: KeychainCredentialStore())
        switch origin {
        case .live(let url):
            // A registered phone signs every request (docs/specs/devices.md).
            let client = MainloopAPI.client(origin: url, middlewares: [AccountSigningMiddleware(account: deviceAccount, origin: url)])
            return AppEnvironment(
                origin: origin, source: LiveSource(client: client),
                account: AccountModel(account: deviceAccount, origin: url, signIn: signIn)
            )
        case .fixtures:
            // Fixtures are bundled with MainloopCore; failing to read them is a build error.
            return AppEnvironment(
                origin: origin, source: try! FixtureSource(),
                account: AccountModel(account: deviceAccount, origin: nil, signIn: nil)
            )
        }
    }

    var isFixtures: Bool { origin == .fixtures }

    var webOrigin: URL? {
        if case .live(let url) = origin { url } else { nil }
    }
}
