import AuthenticationServices
import MainloopCore
import SwiftUI
import UIKit

struct SettingsScreen: View {
    @Environment(AppEnvironment.self) private var environment
    @Environment(AccountModel.self) private var account
    @Environment(PushModel.self) private var push
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
                        if let approvals = account.approvals {
                            LabeledContent("Approvals", value: approvals)
                        } else {
                            LabeledContent("Approvals", value: "Not set up")
                            Text("Sign out and in again to set up approvals. They need a passcode and Touch ID or Face ID.")
                                .font(.footnote)
                                .foregroundStyle(.secondary)
                        }
                        NotificationsRow()
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
                    Text("Signing in registers a key that stays in this iPhone's Secure Enclave and signs every request. A second key, there too, approves privileged actions and asks for Touch ID each time. Signing out revokes the device and deletes both keys, which also stops notifications.")
                }
                Section("About") {
                    LabeledContent("Version", value: Bundle.main.versionString)
                }
            }
            .navigationTitle("Settings")
            .avoidingReservedRegions()
            .task(id: account.phase) {
                await push.refreshPermission()
                await push.refreshServerStatus()
            }
        }
    }
}

/// Whether this iPhone gets notifications, and a way to turn them on.
private struct NotificationsRow: View {
    @Environment(PushModel.self) private var push
    @Environment(\.openURL) private var openURL

    var body: some View {
        switch push.permission {
        case .unknown:
            LabeledContent("Notifications") { ProgressView() }
        case .on:
            LabeledContent("Notifications", value: "On")
        case .off:
            LabeledContent("Notifications", value: "Off")
            Button("Turn on in Settings") {
                if let url = URL(string: UIApplication.openNotificationSettingsURLString) { openURL(url) }
            }
        case .notAsked:
            LabeledContent("Notifications", value: "Not asked yet")
            Button("Turn on") { Task { await push.requestPermission() } }
        }
        if let invalid = push.tokenInvalidAt {
            Text("Apple reported this iPhone's notification token invalid on \(invalid.formatted(date: .abbreviated, time: .shortened)), so Mainloop stopped sending. The app sends its token again each time it opens.")
                .font(.footnote)
                .foregroundStyle(.orange)
        }
        if let error = push.lastError {
            Text(error).font(.footnote).foregroundStyle(.red)
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
