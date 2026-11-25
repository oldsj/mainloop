import Foundation
import MainloopCore
import Testing
@testable import Mainloop

/// First run's phases (docs/specs/ios-app.md, "First run").
@MainActor @Suite struct MainloopAppTests {
    let origin = ServerOrigin.live(URL(string: "https://mainloop.example.org")!)
    let signIn = PocketIDSignIn.fromSettings(issuer: "https://id.example.org", clientID: "mainloop-ios")

    func defaults() -> UserDefaults {
        let name = "MainloopAppTests." + UUID().uuidString
        let defaults = UserDefaults(suiteName: name)!
        defaults.removePersistentDomain(forName: name)
        return defaults
    }

    func paired() -> MemoryCredentialStore {
        MemoryCredentialStore(DeviceCredential(
            deviceID: "dev-0001", origin: URL(string: "https://mainloop.example.org")!, keyKind: .software, keyData: Data(repeating: 1, count: 32)
        ))
    }

    /// The committed build names a placeholder server (Config/Mainloop.xcconfig); a local one may not.
    @Test func aBuildWithoutAServerSaysSo() {
        let named = ServerOrigin.parse(Bundle.main.object(forInfoDictionaryKey: ServerOrigin.infoPlistKey) as? String)
        let app = AppModel.make(origin: .notConfigured, signIn: signIn, apnsEnvironment: nil, store: paired(), defaults: defaults())
        #expect(app.phase == .notConfigured)
        #expect(app.shell == nil)
        #expect(!app.signInConfigured)
        if named == .notConfigured { #expect(AppModel.fromBundle().phase == .notConfigured) }
    }

    @Test func signedOutStartsAtWelcome() {
        let app = AppModel.make(origin: origin, signIn: signIn, apnsEnvironment: nil, store: MemoryCredentialStore(), defaults: defaults())
        #expect(app.phase == .welcome)
        #expect(app.signInConfigured)
        #expect(app.shell != nil)
    }

    @Test func withoutAPocketIDClientWelcomeExplainsIt() {
        let app = AppModel.make(origin: origin, signIn: nil, apnsEnvironment: nil, store: MemoryCredentialStore(), defaults: defaults())
        #expect(app.phase == .welcome)
        #expect(!app.signInConfigured)
    }

    @Test func aPairedPhoneFinishesTheQuickstartOnceThenOpensTheWebApp() {
        let defaults = defaults()
        let app = AppModel.make(origin: origin, signIn: signIn, apnsEnvironment: nil, store: paired(), defaults: defaults)
        #expect(app.phase == .notifications)
        app.finishQuickstart()
        #expect(app.phase == .ready)
        let relaunched = AppModel.make(origin: origin, signIn: signIn, apnsEnvironment: nil, store: paired(), defaults: defaults)
        #expect(relaunched.phase == .ready)
    }

    @Test func aPairingForAnotherServerIsNotThisOne() {
        let other = MemoryCredentialStore(DeviceCredential(
            deviceID: "dev-0001", origin: URL(string: "https://other.example.org")!, keyKind: .software, keyData: Data(repeating: 1, count: 32)
        ))
        let app = AppModel.make(origin: origin, signIn: signIn, apnsEnvironment: nil, store: other, defaults: defaults())
        #expect(app.phase == .welcome)
        #expect(!app.isPaired)
    }

    #if DEBUG
        @Test func debugBuildsOpenWithoutPairingUntilAskedToPair() {
            let defaults = defaults()
            let app = AppModel.make(origin: origin, signIn: signIn, apnsEnvironment: nil, store: MemoryCredentialStore(), defaults: defaults)
            app.openWithoutPairing()
            #expect(app.phase == .ready)
            #expect(AppModel.make(origin: origin, signIn: signIn, apnsEnvironment: nil, store: MemoryCredentialStore(), defaults: defaults).phase == .ready)
            app.pairInstead()
            #expect(app.phase == .welcome)
        }
    #endif
}
