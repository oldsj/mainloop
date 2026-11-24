import MainloopCore
import SwiftUI

struct SettingsScreen: View {
    @Environment(AppEnvironment.self) private var environment

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
                    LabeledContent("Sign-in", value: "Not available yet")
                } header: {
                    Text("This iPhone")
                } footer: {
                    Text("Signing in with Pocket ID, notifications and Touch ID approvals come in the next updates.")
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
