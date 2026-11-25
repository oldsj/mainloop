import AuthenticationServices
import MainloopAPI
import MainloopCore
import SwiftUI

@main
struct MainloopApp: App {
    @UIApplicationDelegateAdaptor(AppDelegate.self) private var appDelegate
    @State private var app = AppModel.shared

    var body: some Scene {
        WindowGroup {
            AppRoot()
                .environment(app)
                .environment(app.account)
                .environment(app.push)
                .preferredColorScheme(.dark)
                .tint(Theme.accent)
        }
    }
}

/// Where the app is (docs/specs/ios-app.md, "First run"): a build without a server says how to set
/// one; otherwise sign in, pair this iPhone, notifications and a short quickstart come before the
/// web app, once per pairing.
@MainActor @Observable
final class AppModel {
    enum Phase: Equatable {
        /// The build names no Mainloop server.
        case notConfigured
        /// Signed out: sign in with Pocket ID to pair this iPhone.
        case welcome
        /// Just paired: what was set up.
        case paired
        case notifications
        case quickstart
        /// The web app.
        case ready
    }

    private(set) var phase: Phase
    /// Why the owner is back at the welcome screen, when it wasn't their choice.
    var notice: String?

    let origin: URL?
    let account: AccountModel
    let push: PushModel
    let approvals: ApprovalModel
    let shell: ShellModel?
    let signInConfigured: Bool

    /// The one model the app and its UIKit delegate share.
    static let shared = AppModel.fromBundle()

    private let defaults: UserDefaults

    init(origin: URL?, account: AccountModel, push: PushModel, approvals: ApprovalModel, shell: ShellModel?, signInConfigured: Bool, defaults: UserDefaults = .standard) {
        self.origin = origin
        self.account = account
        self.push = push
        self.approvals = approvals
        self.shell = shell
        self.signInConfigured = signInConfigured
        self.defaults = defaults
        phase = .notConfigured
        phase = initialPhase()
        shell?.onNotPaired = { [weak self] in self?.lostPairing() }
    }

    static func fromBundle(_ bundle: Bundle = .main) -> AppModel {
        make(
            origin: ServerOrigin.parse(bundle.object(forInfoDictionaryKey: ServerOrigin.infoPlistKey) as? String),
            signIn: PocketIDSignIn.fromSettings(
                issuer: bundle.object(forInfoDictionaryKey: "MainloopOIDCIssuer") as? String,
                clientID: bundle.object(forInfoDictionaryKey: "MainloopOIDCClientID") as? String
            ),
            apnsEnvironment: PushRegistration.environment(
                infoPlistValue: bundle.object(forInfoDictionaryKey: PushRegistration.environmentInfoPlistKey) as? String
            ),
            store: KeychainCredentialStore()
        )
    }

    static func make(
        origin: ServerOrigin, signIn: PocketIDSignIn?, apnsEnvironment: Components.Schemas.APNsEnvironment?,
        store: any CredentialStore, defaults: UserDefaults = .standard
    ) -> AppModel {
        let deviceAccount = DeviceAccount(store: store)
        guard case .live(let url) = origin else {
            let push = PushModel(account: deviceAccount, origin: nil, client: nil, environment: nil)
            return AppModel(
                origin: nil, account: AccountModel(account: deviceAccount, origin: nil, signIn: nil), push: push,
                approvals: ApprovalModel(service: nil, approver: nil), shell: nil, signInConfigured: false, defaults: defaults
            )
        }
        // A paired phone signs every request it makes itself (docs/specs/devices.md).
        let client = MainloopAPI.client(origin: url, middlewares: [AccountSigningMiddleware(account: deviceAccount, origin: url)])
        let push = PushModel(account: deviceAccount, origin: url, client: client, environment: apnsEnvironment)
        let approvals = ApprovalModel(service: LiveApprovalService(client: client), approver: DeviceApprover(account: deviceAccount, origin: url))
        return AppModel(
            origin: url,
            account: AccountModel(account: deviceAccount, origin: url, signIn: signIn),
            push: push,
            approvals: approvals,
            shell: ShellModel(origin: url, client: client, account: deviceAccount, approvals: approvals),
            signInConfigured: signIn != nil,
            defaults: defaults
        )
    }

    private func initialPhase() -> Phase {
        guard origin != nil else { return .notConfigured }
        if case .signedIn(let deviceID) = account.phase {
            return defaults.bool(forKey: Self.onboardedKey(deviceID)) ? .ready : .notifications
        }
        return openedWithoutPairing ? .ready : .welcome
    }

    /// Signs in with Pocket ID and pairs this iPhone.
    func signIn(using session: WebAuthenticationSession) async {
        notice = nil
        if await account.register(using: session) {
            setOpenedWithoutPairing(false)
            phase = .paired
        }
    }

    func continueFromPairing() { phase = .notifications }

    func notifications(allow: Bool) async {
        if allow { await push.requestPermission() }
        phase = .quickstart
    }

    /// The end of the quickstart: from now on this pairing opens straight into the web app.
    func finishQuickstart() {
        if case .signedIn(let deviceID) = account.phase {
            defaults.set(true, forKey: Self.onboardedKey(deviceID))
        }
        phase = .ready
    }

    /// Ends the web sessions, revokes this device, deletes both keys and the web view's data.
    func signOut() async {
        if let shell { await shell.signOut() }
        await account.signOut()
        await push.signedOut()
        setOpenedWithoutPairing(false)
        phase = initialPhase()
    }

    /// The server no longer knows this device (revoked from the web app or another device).
    private func lostPairing() {
        Task {
            await account.signOut()
            await push.signedOut()
            notice = "Mainloop no longer knows this iPhone: it was revoked, or its keys were reset. Sign in to pair it again."
            phase = initialPhase()
        }
    }

    static func onboardedKey(_ deviceID: String) -> String { "onboarded." + deviceID }

    // MARK: Debug: the web app without pairing

    /// Debug builds may open the web app without pairing, for a server whose authentication only
    /// observes. Nothing can be approved, and nothing is signed.
    var canOpenWithoutPairing: Bool {
        #if DEBUG
            origin != nil
        #else
            false
        #endif
    }

    private static let unpairedKey = "openWithoutPairing"
    private var openedWithoutPairing: Bool { canOpenWithoutPairing && defaults.bool(forKey: Self.unpairedKey) }

    func openWithoutPairing() {
        guard canOpenWithoutPairing else { return }
        setOpenedWithoutPairing(true)
        phase = .ready
    }

    /// Back to the welcome screen from the unpaired web app.
    func pairInstead() {
        setOpenedWithoutPairing(false)
        phase = initialPhase()
    }

    private func setOpenedWithoutPairing(_ value: Bool) { defaults.set(value, forKey: Self.unpairedKey) }

    var isPaired: Bool {
        if case .signedIn = account.phase { true } else { false }
    }
}

/// First run until the web app, then the web app.
struct AppRoot: View {
    @Environment(AppModel.self) private var app
    @Environment(PushModel.self) private var push
    @Environment(\.scenePhase) private var scenePhase

    var body: some View {
        ZStack {
            Theme.background.ignoresSafeArea()
            if app.phase == .ready, let shell = app.shell {
                ShellScreen(shell: shell, approvals: app.approvals)
            } else {
                FirstRunView()
            }
        }
        .onChange(of: scenePhase) { _, phase in
            guard phase == .active else { return }
            Task { await push.sync() }
            if app.phase == .ready, app.isPaired { Task { await push.refreshBadge() } }
        }
        .onChange(of: push.pendingRoute, initial: true) { _, route in
            // A tapped notification opens its page in the web app once the app is ready.
            guard let route, app.phase == .ready, let shell = app.shell else { return }
            push.pendingRoute = nil
            shell.open(path: route.webPath)
        }
        .onChange(of: app.phase) { _, phase in
            if phase == .ready, let route = push.pendingRoute, let shell = app.shell {
                push.pendingRoute = nil
                shell.open(path: route.webPath)
            }
        }
    }
}
