import MainloopCore
import SwiftUI

struct NeedsYouScreen: View {
    @Environment(NeedsYouModel.self) private var model

    var body: some View {
        @Bindable var model = model
        ListDetail(title: "Needs you", selection: $model.selection) {
            NeedsYouList()
        } detail: {
            if let item = model.selected { NeedsYouDetail(item: item) }
        } placeholder: {
            ContentUnavailableView("Nothing open", systemImage: "tray", description: Text("Pick an item to answer it."))
        }
        .task { if model.state == .idle { await model.load() } }
    }
}

private struct NeedsYouList: View {
    @Environment(NeedsYouModel.self) private var model

    var body: some View {
        @Bindable var model = model
        List(selection: $model.selection) {
            let decisions = model.items.filter { $0.kind != .problem }
            let problems = model.items.filter { $0.kind == .problem }
            if !decisions.isEmpty {
                Section {
                    ForEach(decisions) { NeedsYouRow(item: $0).tag($0.id) }
                }
            }
            if !problems.isEmpty {
                Section("Problems") {
                    ForEach(problems) { NeedsYouRow(item: $0).tag($0.id) }
                }
            }
        }
        .overlay { emptyOrFailed }
        .refreshable { await model.load() }
    }

    @ViewBuilder private var emptyOrFailed: some View {
        switch model.state {
        case .failed(let message) where model.items.isEmpty:
            ContentUnavailableView {
                Label("Couldn't load", systemImage: "exclamationmark.triangle")
            } description: {
                Text(message)
            } actions: {
                Button("Try again") { Task { await model.load() } }
            }
        case .loaded where model.items.isEmpty:
            ContentUnavailableView("All clear", systemImage: "checkmark.circle", description: Text("Nothing needs you."))
        case .loading where model.items.isEmpty:
            ProgressView()
        default:
            EmptyView()
        }
    }
}

private struct NeedsYouRow: View {
    let item: NeedsYouItem

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 12) {
            Image(systemName: item.symbol)
                .foregroundStyle(item.tint)
                .accessibilityHidden(true)
            VStack(alignment: .leading, spacing: 4) {
                Text(item.title).font(.headline).lineLimit(2)
                Text(item.needed).font(.subheadline).foregroundStyle(.secondary).lineLimit(2)
            }
        }
        .padding(.vertical, 4)
        .accessibilityElement(children: .combine)
        .accessibilityHint(item.privileged != nil ? "Privileged action" : "")
    }
}

extension NeedsYouItem {
    var symbol: String {
        if privileged != nil { return "lock.shield" }
        switch kind {
        case .approval: return "checkmark.seal"
        case .question: return "questionmark.bubble"
        case .ownerDecision: return "arrow.triangle.branch"
        case .review: return "doc.text.magnifyingglass"
        case .problem: return "exclamationmark.triangle"
        }
    }

    var tint: Color {
        if privileged != nil { return .orange }
        return kind == .problem ? .red : .accentColor
    }
}
