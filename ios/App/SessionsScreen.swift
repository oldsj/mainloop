import MainloopCore
import SwiftUI

struct SessionsScreen: View {
    @Environment(SessionsModel.self) private var model

    var body: some View {
        @Bindable var model = model
        ListDetail(title: "Sessions", selection: $model.selection) {
            List(model.rows, selection: $model.selection) { row in
                SessionListRow(row: row).tag(row.id)
            }
            .overlay {
                if case .failed(let message) = model.state, model.rows.isEmpty {
                    ContentUnavailableView("Couldn't load", systemImage: "exclamationmark.triangle", description: Text(message))
                }
            }
            .refreshable { await model.load() }
        } detail: {
            // A new identity per session, as on Needs you; the draft itself lives in the model.
            if let row = model.selected { SessionDetail(row: row).id(row.id) }
        } placeholder: {
            ContentUnavailableView("No session open", systemImage: "bubble.left.and.text.bubble.right")
        }
        .task { if model.state == .idle { await model.load() } }
    }
}

private struct SessionListRow: View {
    let row: SessionRow

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack {
                Text(row.title).font(.headline).lineLimit(1)
                if row.needsOwner {
                    Image(systemName: "circle.fill").font(.caption2).foregroundStyle(.orange)
                        .accessibilityLabel("Needs you")
                }
            }
            Text(row.status.label + (row.agentKind.map { " · \($0.capitalized)" } ?? ""))
                .font(.subheadline).foregroundStyle(.secondary)
        }
        .padding(.vertical, 2)
    }
}

/// The session's summary and a message box. The transcript itself opens in the web app until
/// the native chat slice.
struct SessionDetail: View {
    @Environment(AppEnvironment.self) private var environment
    @Environment(SessionsModel.self) private var model
    let row: SessionRow
    @State private var showWeb = false

    /// The draft belongs to this session in the model (with the `message_id` a retry reuses).
    private var draft: Binding<String> {
        Binding(get: { model.draft(for: row.id) }, set: { model.setDraft($0, for: row.id) })
    }

    var body: some View {
        Form {
            Section {
                Text(row.title).font(.title3.weight(.semibold))
                LabeledContent("Status", value: row.status.label)
                if let summary = row.summary { Text(summary).foregroundStyle(.secondary) }
            }
            Section("Message") {
                TextField("Message \(row.title)", text: draft, axis: .vertical)
                    .lineLimit(1...8)
                Button("Send") {
                    Task { await model.sendDraft(to: row.id) }
                }
                .disabled(model.sending || draft.wrappedValue.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
            }
            if let url = transcriptURL {
                Section {
                    Button("Open transcript") { showWeb = true }
                        .sheet(isPresented: $showWeb) { SafariView(url: url).ignoresSafeArea() }
                }
            }
        }
        .navigationTitle("Session")
        .navigationBarTitleDisplayMode(.inline)
    }

    private var transcriptURL: URL? {
        environment.webOrigin?.appending(path: "sessions").appending(path: row.id)
    }
}
