import MainloopAPI
import MainloopCore
import SwiftUI

@main
struct MainloopApp: App {
    @UIApplicationDelegateAdaptor(AppDelegate.self) private var appDelegate
    @State private var environment = AppEnvironment.shared

    var body: some Scene {
        WindowGroup {
            RootView()
                .environment(environment)
                .environment(environment.needsYou)
                .environment(environment.sessions)
                .environment(environment.account)
                .environment(environment.push)
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
    let push: PushModel

    /// The one environment the app and its UIKit delegate share.
    static let shared = AppEnvironment.fromBundle()

    init(origin: ServerOrigin, source: any MainloopSource, account: AccountModel, push: PushModel) {
        self.origin = origin
        needsYou = NeedsYouModel(source: source)
        sessions = SessionsModel(source: source)
        self.account = account
        self.push = push
    }

    static func fromBundle(_ bundle: Bundle = .main) -> AppEnvironment {
        let origin = ServerOrigin.parse(bundle.object(forInfoDictionaryKey: ServerOrigin.infoPlistKey) as? String)
        let signIn = PocketIDSignIn.fromSettings(
            issuer: bundle.object(forInfoDictionaryKey: "MainloopOIDCIssuer") as? String,
            clientID: bundle.object(forInfoDictionaryKey: "MainloopOIDCClientID") as? String
        )
        let apnsEnvironment = PushRegistration.environment(
            infoPlistValue: bundle.object(forInfoDictionaryKey: PushRegistration.environmentInfoPlistKey) as? String
        )
        let deviceAccount = DeviceAccount(store: KeychainCredentialStore())
        switch origin {
        case .live(let url):
            // A registered phone signs every request (docs/specs/devices.md).
            let client = MainloopAPI.client(origin: url, middlewares: [AccountSigningMiddleware(account: deviceAccount, origin: url)])
            let push = PushModel(account: deviceAccount, origin: url, client: client, environment: apnsEnvironment)
            return AppEnvironment(
                origin: origin, source: LiveSource(client: client),
                account: AccountModel(account: deviceAccount, origin: url, signIn: signIn, push: push),
                push: push
            )
        case .fixtures:
            // Fixtures are bundled with MainloopCore; failing to read them is a build error.
            let push = PushModel(account: deviceAccount, origin: nil, client: nil, environment: nil)
            return AppEnvironment(
                origin: origin, source: try! FixtureSource(),
                account: AccountModel(account: deviceAccount, origin: nil, signIn: nil, push: push),
                push: push
            )
        }
    }

    var isFixtures: Bool { origin == .fixtures }

    var webOrigin: URL? {
        if case .live(let url) = origin { url } else { nil }
    }
}
