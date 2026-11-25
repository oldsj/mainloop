import MainloopCore
import SwiftUI

/// Three tabs. On iPhone Duo the tab bar sits on a side edge except on the inner screen in
/// portrait; the system places it, and `toolbarVerticalBehavior(.automatic)` keeps that default.
struct RootView: View {
    enum Tabs: Hashable { case needsYou, sessions, settings }

    @Environment(AppEnvironment.self) private var environment
    @Environment(NeedsYouModel.self) private var needsYou
    @Environment(PushModel.self) private var push
    @Environment(\.scenePhase) private var scenePhase
    @State private var tab: Tabs = .needsYou
    @State private var webPage: WebPage?

    var body: some View {
        TabView(selection: $tab) {
            Tab("Needs you", systemImage: "tray.full", value: .needsYou) {
                NeedsYouScreen()
            }
            .badge(needsYou.count)

            Tab("Sessions", systemImage: "bubble.left.and.text.bubble.right", value: .sessions) {
                SessionsScreen()
            }

            Tab("Settings", systemImage: "gearshape", value: .settings) {
                SettingsScreen()
            }
        }
        .toolbarVerticalBehavior(.automatic)
        .onChange(of: scenePhase) { _, phase in
            if phase == .active {
                Task { await needsYou.load() }
                Task { await push.sync() }
            }
        }
        // The app icon's badge follows the list, after each load and each answer.
        .onChange(of: needsYou.state) { _, state in
            if state == .loaded { push.setBadge(needsYou.count) }
        }
        .onChange(of: needsYou.count) { _, count in
            if needsYou.state == .loaded { push.setBadge(count) }
        }
        .onChange(of: push.pendingRoute, initial: true) { _, route in
            guard let route else { return }
            push.pendingRoute = nil
            Task { await open(route) }
        }
        .sheet(item: $webPage) { page in
            SafariView(url: page.url).ignoresSafeArea()
        }
    }

    /// Opens what a tapped notification is about.
    private func open(_ route: PushRoute) async {
        switch route {
        case .needsYou:
            tab = .needsYou
        case .needsYouItem(let id):
            tab = .needsYou
            if !needsYou.items.contains(where: { $0.id == id }) { await needsYou.load() }
            // An item answered elsewhere in the meantime is gone: the list stays open instead.
            if needsYou.items.contains(where: { $0.id == id }) { needsYou.selection = id }
        case .webPage(let path):
            if let origin = environment.webOrigin, let url = PushRoute.url(for: path, origin: origin) {
                webPage = WebPage(url: url)
            } else {
                tab = .needsYou
            }
        }
    }
}

/// A web app page opened from a notification.
struct WebPage: Identifiable {
    let url: URL
    var id: URL { url }
}

/// A list beside its detail on the inner screen (regular width), a pushed detail on the outer
/// screen (compact). The split arrangement places its panes around the fold, and the selection
/// lives in the model, so folding or unfolding keeps the open item open.
struct ListDetail<List: View, Detail: View, Placeholder: View>: View {
    @Environment(\.horizontalSizeClass) private var sizeClass
    let title: String
    @Binding var selection: String?
    @ViewBuilder var list: () -> List
    @ViewBuilder var detail: () -> Detail
    @ViewBuilder var placeholder: () -> Placeholder

    var body: some View {
        if sizeClass == .regular {
            ArrangementView {
                NavigationStack {
                    list().navigationTitle(title)
                }
            } secondary: {
                NavigationStack {
                    if selection != nil { detail() } else { placeholder() }
                }
                .avoidingReservedRegions()
            }
            .arrangementViewStyle(.split)
        } else {
            NavigationStack {
                list()
                    .navigationTitle(title)
                    .navigationDestination(isPresented: isPushed) { detail() }
            }
        }
    }

    private var isPushed: Binding<Bool> {
        Binding(get: { selection != nil }, set: { if !$0 { selection = nil } })
    }
}

extension View {
    /// Pads content clear of iPhone Duo's camera regions where the safe area doesn't already.
    /// On other iPhones there are no reserved regions and this adds nothing.
    func avoidingReservedRegions() -> some View {
        modifier(ReservedRegionPadding())
    }
}

private struct ReservedRegionPadding: ViewModifier {
    func body(content: Content) -> some View {
        GeometryReader { proxy in
            let regions = proxy.reservedRegions(kind: .occlusion).filter(\.isActive)
            let needed = EdgeMargins.union(regions.map { EdgeMargins($0.margins) })
            let extra = needed.subtracting(EdgeMargins(proxy.safeAreaInsets))
            content.padding(EdgeInsets(top: extra.top, leading: extra.leading, bottom: extra.bottom, trailing: extra.trailing))
        }
    }
}

extension EdgeMargins {
    init(_ insets: EdgeInsets) {
        self.init(top: insets.top, leading: insets.leading, bottom: insets.bottom, trailing: insets.trailing)
    }
}
