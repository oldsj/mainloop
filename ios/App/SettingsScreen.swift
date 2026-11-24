import AuthenticationServices
import MainloopCore
import SwiftUI

struct SettingsScreen: View {
    @Environment(AppEnvironment.self) private var environment
    @Environment(AccountModel.self) private var account
    @Environment(\.webAuthenticationSession) private var webAuthenticationSession

    var body: some View {
        NavigationStack {
            Form {
                Section("Server") {
                    switch environment.origin {
                    case .live(let url): LabeledContent("Mainloop", value: url.host() ?? url.absoluteString)
                    case .fixtures: LabeledContent("Mainloop", value: "Sample data")
                    }
                }
                Section {
                    switch account.phase {
                    case .unavailable:
                        LabeledContent("Sign-in", value: environment.isFixtures ? "Sample data" : "Not configured")
                    case .signedOut:
                        Button("Sign in with Pocket ID") {
                            Task { await account.register(using: webAuthenticationSession) }
                        }
                    case .working:
                        HStack {
                            Text("Working…")
                            Spacer()
                            ProgressView()
                        }
                    case .signedIn(let deviceID):
                        LabeledContent("Device", value: deviceID)
                            .textSelection(.enabled)
                        Button("Sign out", role: .destructive) {
                            Task { await account.signOut() }
                        }
                    }
                    if let error = account.lastError {
                        Text(error).foregroundStyle(.red)
                    }
                } header: {
                    Text("This iPhone")
                } footer: {
                    Text("Signing in registers a key that stays in this iPhone's Secure Enclave and signs every request. Signing out revokes it. Notifications and Touch ID approvals come in the next updates.")
                }
                Section("About") {
                    LabeledContent("Version", value: Bundle.main.versionString)
                }
            }
            .navigationTitle("Settings")
            .avoidingReservedRegions()
        }
    }
}

extension Bundle {
    var versionString: String {
        let version = object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "?"
        let build = object(forInfoDictionaryKey: "CFBundleVersion") as? String ?? "?"
        return "\(version) (\(build))"
    }
}
