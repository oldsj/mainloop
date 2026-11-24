import MainloopCore
import SwiftUI

/// Three tabs. On iPhone Duo the tab bar sits on a side edge except on the inner screen in
/// portrait; the system places it, and `toolbarVerticalBehavior(.automatic)` keeps that default.
struct RootView: View {
    @Environment(NeedsYouModel.self) private var needsYou
    @Environment(\.scenePhase) private var scenePhase

    var body: some View {
        TabView {
            Tab("Needs you", systemImage: "tray.full") {
                NeedsYouScreen()
            }
            .badge(needsYou.count)

            Tab("Sessions", systemImage: "bubble.left.and.text.bubble.right") {
                SessionsScreen()
            }

            Tab("Settings", systemImage: "gearshape") {
                SettingsScreen()
            }
        }
        .toolbarVerticalBehavior(.automatic)
        .onChange(of: scenePhase) { _, phase in
            if phase == .active { Task { await needsYou.load() } }
        }
    }
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
