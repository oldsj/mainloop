import MainloopCore
import SwiftUI
import UIKit

/// This iPhone, opened from the web app's Settings: the device, how it approves, notifications,
/// and Sign out (which ends its web sessions, revokes it and deletes both keys).
struct DeviceScreen: View {
    @Environment(AppModel.self) private var app
    @Environment(AccountModel.self) private var account
    @Environment(PushModel.self) private var push
    @Environment(\.dismiss) private var dismiss
    @Environment(\.openURL) private var openURL
    @State private var confirmSignOut = false
    @State private var signingOut = false

    var body: some View {
        VStack(spacing: 0) {
            HStack {
                Text("# this iphone").font(Theme.mono(.headline, weight: .semibold)).foregroundStyle(Theme.foreground)
                Spacer()
                Button("Done") { dismiss() }
                    .font(Theme.mono(.body, weight: .medium))
                    .foregroundStyle(Theme.accent)
                    .frame(minHeight: Theme.control)
            }
            .padding(.horizontal, Theme.Space.md)
            .overlay(alignment: .bottom) { Rectangle().fill(Theme.borderSubtle).frame(height: 1) }
            ScrollView {
                VStack(alignment: .leading, spacing: Theme.Space.md) {
                    if app.isPaired { paired } else { unpaired }
                }
                .padding(Theme.Space.md)
            }
        }
        .background(Theme.background.ignoresSafeArea())
        .preferredColorScheme(.dark)
        .task {
            await push.refreshPermission()
            await push.refreshServerStatus()
        }
    }

    @ViewBuilder private var paired: some View {
        Panel(title: "device") {
            FactRow(label: "name", value: UIDevice.current.name)
            FactRow(label: "server", value: app.origin?.host() ?? "")
            FactRow(label: "request key", value: account.requestKey ?? "Missing")
        }
        Panel(title: "approvals") {
            FactRow(label: "approval key", value: account.approvals ?? "Not set up: sign out and in again")
        }
        Panel(title: "notifications") {
            FactRow(label: "permission", value: permission, tint: push.permission == .on ? Theme.green : Theme.foreground)
            if let invalid = push.tokenInvalidAt {
                Notice(text: "Apple stopped accepting this iPhone's notification token on \(invalid.formatted(date: .abbreviated, time: .shortened)). It's sent again the next time the app opens.")
            }
            if let error = push.lastError { Notice(text: error, tint: Theme.error) }
            switch push.permission {
            case .notAsked:
                Button("Turn on notifications") { Task { await push.requestPermission() } }
                    .buttonStyle(TerminalButtonStyle())
            case .off:
                Button("Open iOS Settings") {
                    if let url = URL(string: UIApplication.openSettingsURLString) { openURL(url) }
                }
                .buttonStyle(TerminalButtonStyle())
            case .on, .unknown:
                EmptyView()
            }
        }
        Button {
            confirmSignOut = true
        } label: {
            if signingOut { ProgressView().tint(Theme.error) } else { Text("Sign out") }
        }
        .buttonStyle(TerminalButtonStyle(destructive: true))
        .disabled(signingOut)
        .confirmationDialog("Sign out of Mainloop on this iPhone?", isPresented: $confirmSignOut, titleVisibility: .visible) {
            Button("Sign out", role: .destructive) {
                signingOut = true
                Task {
                    await app.signOut()
                    signingOut = false
                    dismiss()
                }
            }
        } message: {
            Text("This ends its web sessions, revokes this iPhone and deletes both of its keys. Pairing again makes new ones.")
        }
    }

    @ViewBuilder private var unpaired: some View {
        Notice(text: "This iPhone isn't paired: the web app works as in a browser, and nothing can be approved here.")
        Button("Pair this iPhone") {
            dismiss()
            app.pairInstead()
        }
        .buttonStyle(TerminalButtonStyle(prominent: true))
    }

    private var permission: String {
        switch push.permission {
        case .unknown: "…"
        case .notAsked: "Not asked yet"
        case .off: "Off in iOS Settings"
        case .on: "On"
        }
    }
}
