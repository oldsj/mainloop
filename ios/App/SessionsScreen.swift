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
            if let row = model.selected { SessionDetail(row: row) }
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
    @State private var draft = ""
    /// Kept until the send succeeds, so a retry of the same text is one message on the server.
    @State private var messageID = UUID().uuidString
    @State private var showWeb = false

    var body: some View {
        Form {
            Section {
                Text(row.title).font(.title3.weight(.semibold))
                LabeledContent("Status", value: row.status.label)
                if let summary = row.summary { Text(summary).foregroundStyle(.secondary) }
            }
            Section("Message") {
                TextField("Message \(row.title)", text: $draft, axis: .vertical)
                    .lineLimit(1...8)
                    .onChange(of: draft) { messageID = UUID().uuidString }
                Button("Send") {
                    Task {
                        if await model.send(draft, to: row.id, messageID: messageID) {
                            draft = ""
                            messageID = UUID().uuidString
                        }
                    }
                }
                .disabled(model.sending || draft.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
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
