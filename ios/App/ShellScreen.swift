import MainloopAPI
import MainloopCore
import SwiftUI
import WebKit

/// The web app in a web view (docs/specs/ios-app.md, "The web app in the app"). The page is
/// Mainloop's own web app; the app adds only what must stay native:
///
/// - a paired phone's web session: a device-signed `POST /devices/current/web-session` gives a
///   short token, set as the host-only HttpOnly cookie `__Host-mainloop-device` before the page
///   loads. The page never sees a key or a signature, and can't call the device's signed-only
///   routes (push, web sessions, revoke).
/// - one message handler, `mainloop`, answered only for the main frame on Mainloop's origin.
/// - navigation: the main frame stays on the origin; other web pages open in a Safari sheet;
///   anything else (other schemes, `javascript:`) is refused.
///
/// The web view keeps the default (persistent) website data store, so the page's own settings,
/// such as its theme, survive a relaunch. The session cookie has no expiry and is set again on each
/// launch; Sign out ends the session on the server and clears the store.
@MainActor @Observable
final class ShellModel {
    enum State: Equatable {
        case starting
        case ready
        /// Couldn't open: a sentence that says what to do, never a raw error.
        case unavailable(String)
    }

    private(set) var state: State = .starting
    /// The page's first load finished (until then the mark shows).
    private(set) var loaded = false
    var showDevice = false
    var outside: OutsideLink?

    let origin: URL
    let approvals: ApprovalModel
    /// Called when the server no longer knows this device.
    var onNotPaired: (() -> Void)?

    private let client: Client
    private let account: DeviceAccount
    private var grant: WebSessionGrant?
    private var pendingPath: String?
    private var renewal: Task<Void, Never>?
    private var starting = false
    private var expiries = SessionExpiryLimit()

    @ObservationIgnored private(set) lazy var webView: WKWebView = makeWebView()
    @ObservationIgnored private lazy var bridge = ShellBridge(shell: self)
    @ObservationIgnored private lazy var navigation = ShellNavigationDelegate(shell: self)

    init(origin: URL, client: Client, account: DeviceAccount, approvals: ApprovalModel) {
        self.origin = origin
        self.client = client
        self.account = account
        self.approvals = approvals
    }

    var isPaired: Bool { account.credential?.origin == origin }

    private func makeWebView() -> WKWebView {
        let configuration = WKWebViewConfiguration()
        configuration.websiteDataStore = .default()
        configuration.userContentController.addScriptMessageHandler(bridge, contentWorld: .page, name: ShellMessage.handlerName)
        configuration.allowsInlineMediaPlayback = true
        configuration.preferences.javaScriptCanOpenWindowsAutomatically = false
        let view = WKWebView(frame: .zero, configuration: configuration)
        view.navigationDelegate = navigation
        view.uiDelegate = navigation
        view.allowsBackForwardNavigationGestures = true
        view.isOpaque = false
        view.backgroundColor = Theme.uiBackground
        view.scrollView.backgroundColor = Theme.uiBackground
        // The page places itself inside the safe area; the scroll view adds no insets of its own.
        view.scrollView.contentInsetAdjustmentBehavior = .never
        #if DEBUG
            view.isInspectable = true
        #endif
        return view
    }

    // MARK: Session

    /// Opens the web app: a paired phone starts its web session first. Unpaired (Debug only), the
    /// page loads as any browser's would.
    func start() async {
        guard !starting else { return }
        starting = true
        defer { starting = false }
        state = .starting
        if isPaired {
            guard await renew() else { return }
        }
        state = .ready
        load(pendingPath ?? "/")
        pendingPath = nil
    }

    /// Back in the foreground: a session with ten minutes or less left is replaced.
    func foregrounded() async {
        guard state == .ready, isPaired, grant?.needsRenewal() ?? true else { return }
        await renew()
    }

    /// The page got a 401: a new session, then the page again.
    /// At most one new session per 30 seconds, so a page that keeps getting 401s can't keep
    /// minting sessions.
    func sessionExpired() async {
        guard isPaired, expiries.allow() else { return }
        if await renew() { webView.reload() }
    }

    /// Starts a session and sets its cookie. A new session ends this device's earlier ones.
    @discardableResult
    private func renew() async -> Bool {
        do {
            let grant = try await WebSessionGrant.start(client: client)
            guard let cookie = grant.cookie(for: origin) else { throw WebSessionError.malformed }
            await webView.configuration.websiteDataStore.httpCookieStore.setCookie(cookie)
            self.grant = grant
            scheduleRenewal(grant)
            return true
        } catch WebSessionError.notPaired {
            state = .unavailable("Mainloop no longer knows this iPhone.")
            onNotPaired?()
        } catch is URLError {
            state = .unavailable("Couldn't reach \(origin.host() ?? "Mainloop"). It runs on your home network: connect to it or its VPN, then try again.")
        } catch {
            state = .unavailable("Mainloop couldn't open a session for this iPhone. Try again; if it keeps failing, check that the server runs a version with device web sessions.")
        }
        return false
    }

    /// Renews ten minutes before the session ends while the app stays open.
    private func scheduleRenewal(_ grant: WebSessionGrant) {
        renewal?.cancel()
        let wait = max(grant.expiresAt.timeIntervalSinceNow - WebSessionGrant.renewWithin, 60)
        renewal = Task { [weak self] in
            try? await Task.sleep(for: .seconds(wait))
            guard !Task.isCancelled, let self, self.grant == grant else { return }
            await self.renew()
        }
    }

    /// Ends this device's web sessions on the server and forgets the web view's data.
    func signOut() async {
        if grant != nil { await WebSessionGrant.end(client: client) }
        await forget()
    }

    /// Forgets the session and the web view's data without asking the server: for a device the
    /// server no longer knows, whose sessions are already gone.
    func forget() async {
        renewal?.cancel()
        renewal = nil
        grant = nil
        expiries = SessionExpiryLimit()
        loaded = false
        state = .starting
        await WKWebsiteDataStore.default().removeData(ofTypes: WKWebsiteDataStore.allWebsiteDataTypes(), modifiedSince: .distantPast)
        webView.load(URLRequest(url: URL(string: "about:blank")!))
    }

    // MARK: Pages

    /// Opens a page of the web app (a tapped notification); before the session starts, once it has.
    func open(path: String) {
        guard state == .ready else {
            pendingPath = path
            return
        }
        load(path)
    }

    private func load(_ path: String) {
        guard let url = URL(string: path, relativeTo: origin)?.absoluteURL, ShellNavigation.isSameOrigin(url, origin) else {
            webView.load(URLRequest(url: origin))
            return
        }
        webView.load(URLRequest(url: url))
    }

    fileprivate func finishedLoading() { loaded = true }

    fileprivate func failed(_ error: any Error) {
        let error = error as NSError
        // A navigation the app cancelled or replaced isn't a failure.
        if error.domain == NSURLErrorDomain, error.code == NSURLErrorCancelled { return }
        if error.domain == "WebKitErrorDomain", error.code == 102 { return }
        guard !loaded else { return }
        state = .unavailable("Couldn't reach \(origin.host() ?? "Mainloop"). It runs on your home network: connect to it or its VPN, then try again.")
    }

    fileprivate func contentProcessEnded() { webView.reload() }

    fileprivate func openOutside(_ url: URL) {
        guard let scheme = url.scheme?.lowercased(), scheme == "https" || scheme == "http" else { return }
        outside = OutsideLink(url: url)
    }

    // MARK: Bridge

    /// Answers a message from the page. Only the main frame on Mainloop's origin is answered; the
    /// approval is read and hashed here, never taken from the page.
    fileprivate func receive(_ message: WKScriptMessage) async -> (Any?, String?) {
        let frame = message.frameInfo
        let source = frame.securityOrigin
        guard message.webView === webView, frame.isMainFrame,
              ShellNavigation.isOrigin(scheme: source.protocol, host: source.host, port: source.port, of: origin),
              let url = frame.request.url, ShellNavigation.isSameOrigin(url, origin)
        else { return (nil, "Not available here.") }
        switch ShellMessage(body: message.body) {
        case .approve(let request):
            return (await approvals.request(request).reply, nil)
        case .sessionExpired:
            await sessionExpired()
            return (nil, nil)
        case .openDeviceSettings:
            showDevice = true
            return (nil, nil)
        case nil:
            return (nil, "Unknown message.")
        }
    }
}

struct OutsideLink: Identifiable {
    let url: URL
    var id: URL { url }
}

/// The one message handler. It holds the model weakly: the web view's configuration keeps it.
private final class ShellBridge: NSObject, WKScriptMessageHandlerWithReply {
    weak var shell: ShellModel?

    init(shell: ShellModel) { self.shell = shell }

    @MainActor
    func userContentController(
        _ userContentController: WKUserContentController,
        didReceive message: WKScriptMessage,
        replyHandler: @escaping @MainActor @Sendable (Any?, String?) -> Void
    ) {
        guard let shell else { return replyHandler(nil, "Not available here.") }
        Task { @MainActor in
            let (reply, error) = await shell.receive(message)
            replyHandler(reply, error)
        }
    }
}

/// Where the page may go (`ShellNavigation`), and the load's progress.
private final class ShellNavigationDelegate: NSObject, WKNavigationDelegate, WKUIDelegate {
    weak var shell: ShellModel?

    init(shell: ShellModel) { self.shell = shell }

    @MainActor
    func webView(_ webView: WKWebView, decidePolicyFor action: WKNavigationAction) async -> WKNavigationActionPolicy {
        guard let shell, let url = action.request.url else { return .cancel }
        // No target frame is a new window; it is decided as the main frame would be.
        let mainFrame = action.targetFrame?.isMainFrame ?? true
        if url.absoluteString == "about:blank", mainFrame, !shell.loaded { return .allow }
        switch ShellNavigation.decide(url, origin: shell.origin, mainFrame: mainFrame) {
        case .load:
            return .allow
        case .openOutside:
            if action.navigationType == .linkActivated || action.targetFrame == nil { shell.openOutside(url) }
            return .cancel
        case .refuse:
            return .cancel
        }
    }

    /// `target="_blank"` and `window.open`: Mainloop's pages load here, others in Safari.
    @MainActor
    func webView(_ webView: WKWebView, createWebViewWith configuration: WKWebViewConfiguration, for action: WKNavigationAction, windowFeatures: WKWindowFeatures) -> WKWebView? {
        guard let shell, let url = action.request.url else { return nil }
        switch ShellNavigation.decide(url, origin: shell.origin, mainFrame: true) {
        case .load: webView.load(URLRequest(url: url))
        case .openOutside: shell.openOutside(url)
        case .refuse: break
        }
        return nil
    }

    @MainActor
    func webView(_ webView: WKWebView, didFinish navigation: WKNavigation!) {
        if webView.url?.absoluteString != "about:blank" { shell?.finishedLoading() }
    }

    @MainActor
    func webView(_ webView: WKWebView, didFailProvisionalNavigation navigation: WKNavigation!, withError error: any Error) {
        shell?.failed(error)
    }

    @MainActor
    func webView(_ webView: WKWebView, didFail navigation: WKNavigation!, withError error: any Error) {
        shell?.failed(error)
    }

    @MainActor
    func webViewWebContentProcessDidTerminate(_ webView: WKWebView) {
        shell?.contentProcessEnded()
    }
}

private struct WebView: UIViewRepresentable {
    let view: WKWebView

    func makeUIView(context: Context) -> WKWebView { view }
    func updateUIView(_ uiView: WKWebView, context: Context) {}
}

/// The web app, with the native sheets it opens.
struct ShellScreen: View {
    @Bindable var shell: ShellModel
    @Bindable var approvals: ApprovalModel
    @Environment(AppModel.self) private var app
    @Environment(\.scenePhase) private var scenePhase

    var body: some View {
        ZStack {
            Theme.background.ignoresSafeArea()
            // Edge to edge: the page declares viewport-fit=cover and pads itself with
            // env(safe-area-inset-*), including iPhone Duo's status bar column on the right edge.
            WebView(view: shell.webView)
                .ignoresSafeArea()
                .opacity(shell.loaded ? 1 : 0)
            switch shell.state {
            case .unavailable(let message):
                Unavailable(message: message) { Task { await shell.start() } }
            case .starting, .ready:
                if !shell.loaded {
                    VStack(spacing: Theme.Space.lg) {
                        Mark(size: 72)
                        ProgressView().tint(Theme.muted)
                    }
                }
            }
        }
        .task { if shell.state == .starting { await shell.start() } }
        .onChange(of: scenePhase) { _, phase in
            if phase == .active { Task { await shell.foregrounded() } }
        }
        .sheet(item: $shell.outside) { SafariView(url: $0.url).ignoresSafeArea() }
        .sheet(isPresented: $shell.showDevice) { DeviceScreen() }
        .background {
            // A separate host, so the approval sheet can open over the device screen's.
            Color.clear.sheet(item: Binding(get: { approvals.reviewing }, set: { if $0 == nil { approvals.cancel() } })) {
                ApprovalSheet(review: $0)
            }
        }
    }
}

/// The server couldn't be reached or refused: what to do, and Retry.
private struct Unavailable: View {
    let message: String
    let retry: () -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: Theme.Space.lg) {
            Mark(size: 56)
            Text("Mainloop isn't available").font(Theme.mono(.title3, weight: .semibold)).foregroundStyle(Theme.foreground)
            Notice(text: message)
            Button("Try again", action: retry).buttonStyle(TerminalButtonStyle(prominent: true))
        }
        .padding(Theme.Space.lg)
        .frame(maxWidth: 520)
        .background(Theme.background)
    }
}
