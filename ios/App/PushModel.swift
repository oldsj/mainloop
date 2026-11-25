import MainloopAPI
import MainloopCore
import SwiftUI
import UIKit
import UserNotifications

/// Notifications through APNs (docs/specs/ios-app.md, "Notifications"): permission, this phone's
/// token on the server, the badge, and where a tapped notification goes.
@MainActor @Observable
final class PushModel {
    enum Permission: Equatable {
        case unknown
        case notAsked
        case off
        case on
    }

    private(set) var permission: Permission = .unknown
    /// Why the last registration failed, as Settings shows it.
    private(set) var lastError: String?
    /// When APNs told the server this phone's token stopped working; nil while it works.
    private(set) var tokenInvalidAt: Date?
    /// Set by a tapped notification; AppRoot opens its page in the web app and clears it.
    var pendingRoute: PushRoute?

    private let account: DeviceAccount
    private let origin: URL?
    private let client: Client?
    private let environment: Components.Schemas.APNsEnvironment?
    /// The device and token set on the server (or being set), so repeated hand-offs from iOS in
    /// one run send once.
    private var sent: (deviceID: String, token: Data)?

    /// `client` is the app's signing client for `origin`; both are nil in a build that names no
    /// server, which never registers.
    init(account: DeviceAccount, origin: URL?, client: Client?, environment: Components.Schemas.APNsEnvironment?) {
        self.account = account
        self.origin = origin
        self.client = client
        self.environment = environment
    }

    /// Registered with this app's origin, as AccountModel counts it.
    private var signedIn: Bool {
        guard client != nil, let origin, let credential = account.credential else { return false }
        return credential.origin == origin
    }

    private var center: UNUserNotificationCenter { .current() }

    /// The categories the server sets. When the owner hides previews, the lock screen shows the
    /// placeholder instead of the text.
    static let categories: Set<UNNotificationCategory> = [
        // No actions: a signed Approve asks for Touch ID, which needs the app in the foreground,
        // so a tap opens the card in the app.
        UNNotificationCategory(
            identifier: PushCategory.needsYou, actions: [], intentIdentifiers: [],
            hiddenPreviewsBodyPlaceholder: "Mainloop needs you", options: []
        ),
        UNNotificationCategory(
            identifier: PushCategory.task, actions: [], intentIdentifiers: [],
            hiddenPreviewsBodyPlaceholder: "A task changed", options: []
        ),
    ]

    /// On launch and each return to the foreground: read the permission, and while signed in
    /// with it granted, ask iOS for the token (which arrives in `received(token:)`).
    func sync() async {
        await refreshPermission()
        if signedIn, permission == .on {
            UIApplication.shared.registerForRemoteNotifications()
        }
    }

    func refreshPermission() async {
        let settings = await center.notificationSettings()
        permission = switch settings.authorizationStatus {
        case .notDetermined: .notAsked
        case .denied: .off
        case .authorized, .provisional, .ephemeral: .on
        @unknown default: .off
        }
    }

    /// Asks once (iOS shows its prompt only the first time), then registers if allowed. Only
    /// while signed in: a signed-out phone has nowhere to send a token.
    func requestPermission() async {
        guard signedIn else { return }
        do {
            _ = try await center.requestAuthorization(options: [.alert, .sound, .badge])
        } catch {
            lastError = "Couldn't ask for notifications: \(error.localizedDescription)"
        }
        await sync()
    }

    func received(token: Data) {
        guard signedIn, let client, let deviceID = account.credential?.deviceID else { return }
        guard let environment else {
            lastError = "This build doesn't name an APNs environment."
            return
        }
        if let sent, sent.deviceID == deviceID, sent.token == token { return }
        // Claimed before the request, so a second hand-off while it is in flight sends nothing;
        // a failure releases it for the next one.
        sent = (deviceID, token)
        Task {
            do {
                let push = try await PushRegistration.send(token: token, environment: environment, client: client)
                tokenInvalidAt = push?.invalidAt
                lastError = nil
            } catch {
                if sent?.deviceID == deviceID, sent?.token == token { sent = nil }
                lastError = Self.describe(error)
            }
        }
    }

    func registrationFailed(_ error: any Error) {
        lastError = "iOS couldn't register for notifications: \(error.localizedDescription)"
    }

    /// Reads whether the server still has a working token for this phone.
    func refreshServerStatus() async {
        guard signedIn, let client else { return }
        do {
            tokenInvalidAt = try await PushRegistration.status(client: client)?.invalidAt
        } catch {
            // Offline or refused: keep what was last known.
        }
    }

    /// The app's badge follows the inbox's count (`GET /attention`) while signed in.
    func refreshBadge() async {
        guard signedIn, let client, let count = try? await AttentionBadge.count(client: client) else { return }
        try? await center.setBadgeCount(count)
    }

    /// Signing out: the phone leaves APNs (a token left on the server is refused from then on, and
    /// the revoke that signing out sends clears it there too), no badge, no notifications left in
    /// Notification Center, and the next sign-in registers and sends a new token.
    func signedOut() async {
        UIApplication.shared.unregisterForRemoteNotifications()
        sent = nil
        tokenInvalidAt = nil
        lastError = nil
        pendingRoute = nil
        center.removeAllDeliveredNotifications()
        try? await center.setBadgeCount(0)
    }

    private static func describe(_ error: any Error) -> String {
        switch error {
        case PushError.signedOut: "Mainloop didn't recognise this iPhone. Sign out and in again."
        case PushError.refused(let status): "Mainloop refused this iPhone's notification token (\(status))."
        default: "Couldn't send the notification token: \(error.localizedDescription)"
        }
    }
}

/// UIKit's hooks for remote notifications. The app's state lives in `AppModel.shared`, so
/// a notification tapped before the first screen appears still finds it.
@MainActor
final class AppDelegate: NSObject, UIApplicationDelegate, UNUserNotificationCenterDelegate {
    func application(
        _ application: UIApplication,
        didFinishLaunchingWithOptions launchOptions: [UIApplication.LaunchOptionsKey: Any]? = nil
    ) -> Bool {
        // The delegate must be set before launch finishes to receive the tap that launched the app.
        let center = UNUserNotificationCenter.current()
        center.delegate = self
        center.setNotificationCategories(PushModel.categories)
        let push = AppModel.shared.push
        Task { await push.sync() }
        return true
    }

    func application(_ application: UIApplication, didRegisterForRemoteNotificationsWithDeviceToken deviceToken: Data) {
        AppModel.shared.push.received(token: deviceToken)
    }

    func application(_ application: UIApplication, didFailToRegisterForRemoteNotificationsWithError error: any Error) {
        AppModel.shared.push.registrationFailed(error)
    }

    /// In the foreground: show it as usual, and refresh the badge.
    nonisolated func userNotificationCenter(
        _ center: UNUserNotificationCenter,
        willPresent notification: UNNotification,
        withCompletionHandler completionHandler: @escaping (UNNotificationPresentationOptions) -> Void
    ) {
        completionHandler([.banner, .list, .sound, .badge])
        Task { @MainActor in await AppModel.shared.push.refreshBadge() }
    }

    /// A tap opens what the notification is about.
    nonisolated func userNotificationCenter(
        _ center: UNUserNotificationCenter,
        didReceive response: UNNotificationResponse,
        withCompletionHandler completionHandler: @escaping () -> Void
    ) {
        if response.actionIdentifier == UNNotificationDefaultActionIdentifier {
            let route = PushRoute(userInfo: response.notification.request.content.userInfo)
            Task { @MainActor in AppModel.shared.push.pendingRoute = route }
        }
        completionHandler()
    }
}
