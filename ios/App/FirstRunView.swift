import AuthenticationServices
import MainloopCore
import SwiftUI

/// First run (docs/specs/ios-app.md): a build without a server says how to set one; otherwise sign
/// in, see what pairing set up, choose notifications, a three-card quickstart, then the web app.
struct FirstRunView: View {
    @Environment(AppModel.self) private var app

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: Theme.Space.lg) {
                switch app.phase {
                case .notConfigured: NotConfigured()
                case .welcome: Welcome()
                case .paired: Paired()
                case .notifications: Notifications()
                case .quickstart: Quickstart()
                case .ready: Welcome() // An unpaired build with no web view; never shown in practice.
                }
            }
            .padding(Theme.Space.lg)
            .frame(maxWidth: 560, alignment: .leading)
            .frame(maxWidth: .infinity)
        }
        .scrollBounceBehavior(.basedOnSize)
        .background(Theme.background.ignoresSafeArea())
        .animation(.default, value: app.phase)
    }
}

/// The mark and a heading, as the top of each step.
private struct StepHeader: View {
    let title: String
    let lead: String

    var body: some View {
        VStack(alignment: .leading, spacing: Theme.Space.md) {
            Mark(size: 64).padding(.top, Theme.Space.xl)
            Text(title).font(Theme.mono(.title2, weight: .semibold)).foregroundStyle(Theme.foreground)
            Text(lead).font(Theme.mono(.callout)).foregroundStyle(Theme.muted)
        }
    }
}

private struct NotConfigured: View {
    var body: some View {
        StepHeader(title: "No server in this build", lead: "This build of Mainloop doesn't name a server, so there is nothing to sign in to.")
        Panel(title: "to fix") {
            Text("Set MAINLOOP_ORIGIN to your Mainloop's https:// address in ios/Config/Local.xcconfig, with the Pocket ID issuer and the iPhone app's client id, then build again.")
                .font(Theme.mono(.callout)).foregroundStyle(Theme.foreground)
            Text("ios/README.md lists every setting.").font(Theme.mono(.caption)).foregroundStyle(Theme.muted)
        }
    }
}

private struct Welcome: View {
    @Environment(AppModel.self) private var app
    @Environment(AccountModel.self) private var account
    @Environment(\.webAuthenticationSession) private var session

    var body: some View {
        StepHeader(title: "mainloop", lead: "Your agents' work, what needs you, and approvals, on this iPhone.")
        if let notice = app.notice { Notice(text: notice) }
        if app.signInConfigured {
            Panel(title: "pair this iPhone") {
                Text("Sign in with Pocket ID. This iPhone makes two keys in its Secure Enclave: one signs its requests, the other signs approvals and asks for Touch ID each time. Neither leaves the phone.")
                    .font(Theme.mono(.callout)).foregroundStyle(Theme.foreground)
            }
            if let error = account.lastError { Notice(text: error, tint: Theme.error) }
            Button {
                Task { await app.signIn(using: session) }
            } label: {
                if account.phase == .working {
                    ProgressView().tint(Theme.background)
                } else {
                    Text("Sign in with Pocket ID")
                }
            }
            .buttonStyle(TerminalButtonStyle(prominent: true))
            .disabled(account.phase == .working)
        } else {
            Panel(title: "sign-in isn't set up") {
                Text("This build names a server but no Pocket ID client, so it can't pair. Set MAINLOOP_OIDC_ISSUER and MAINLOOP_OIDC_CLIENT_ID in ios/Config/Local.xcconfig; the server needs the same client in MAINLOOP_AUTH_DEVICE_CLIENT_ID.")
                    .font(Theme.mono(.callout)).foregroundStyle(Theme.foreground)
            }
        }
        if app.canOpenWithoutPairing {
            Button("Open without pairing (debug)") { app.openWithoutPairing() }
                .buttonStyle(TerminalButtonStyle())
            Text("Debug builds only. The web app works as it does in a browser; nothing can be approved from this iPhone.")
                .font(Theme.mono(.caption)).foregroundStyle(Theme.muted)
        }
    }
}

private struct Paired: View {
    @Environment(AppModel.self) private var app
    @Environment(AccountModel.self) private var account

    var body: some View {
        StepHeader(title: "This iPhone is paired", lead: "Mainloop knows this iPhone by its key. You can revoke it from This iPhone here, or from the web app's devices.")
        Panel(title: "set up", tint: Theme.green) {
            FactRow(label: "request key", value: account.requestKey ?? "Missing")
            FactRow(label: "approvals", value: account.approvals ?? "Not set up: sign out and in again")
            FactRow(label: "server", value: app.origin?.host() ?? "")
        }
        Button("Continue") { app.continueFromPairing() }
            .buttonStyle(TerminalButtonStyle(prominent: true))
    }
}

private struct Notifications: View {
    @Environment(AppModel.self) private var app
    @State private var asking = false

    var body: some View {
        StepHeader(title: "Notifications", lead: "Mainloop tells this iPhone when something needs you: a question, a review, an approval. A tap opens it.")
        Panel(title: "what they carry") {
            Text("A title and a line, never a secret. Approving still happens in the app, after Touch ID.")
                .font(Theme.mono(.callout)).foregroundStyle(Theme.foreground)
        }
        Button {
            asking = true
            Task {
                await app.notifications(allow: true)
                asking = false
            }
        } label: {
            if asking { ProgressView().tint(Theme.background) } else { Text("Turn on notifications") }
        }
        .buttonStyle(TerminalButtonStyle(prominent: true))
        .disabled(asking)
        Button("Not now") { Task { await app.notifications(allow: false) } }
            .buttonStyle(TerminalButtonStyle())
            .disabled(asking)
    }
}

private struct Quickstart: View {
    @Environment(AppModel.self) private var app

    private let cards: [(String, String)] = [
        ("inbox", "What needs you: agents' questions, reviews and approvals. Answer from the card."),
        ("approve", "A privileged action's Approve opens this app's own sheet: it shows the action as this iPhone read it, then asks for Touch ID."),
        ("this iphone", "Settings → This iPhone shows this device, its keys and notifications, and signs it out."),
    ]

    var body: some View {
        StepHeader(title: "Quickstart", lead: "The app is Mainloop's web app, with three things only this iPhone does.")
        ForEach(Array(cards.enumerated()), id: \.offset) { index, card in
            Panel(title: "\(index + 1) \(card.0)") {
                Text(card.1).font(Theme.mono(.callout)).foregroundStyle(Theme.foreground)
            }
        }
        Button("Open Mainloop") { app.finishQuickstart() }
            .buttonStyle(TerminalButtonStyle(prominent: true))
    }
}
