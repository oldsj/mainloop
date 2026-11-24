import MainloopCore
import SwiftUI

/// One card: what is asked, the facts, and the answers. A privileged card shows the action id
/// and the digest prefix as the web card does; approving it on the phone waits for signed
/// approvals (Touch ID), so only Deny and a note are offered here.
struct NeedsYouDetail: View {
    @Environment(NeedsYouModel.self) private var model
    let item: NeedsYouItem

    /// The note belongs to this card in the model, never to whichever card is open.
    private var reply: Binding<String> {
        Binding(get: { model.replies[item.id] ?? "" }, set: { model.replies[item.id] = $0 })
    }

    private var busy: Bool { model.answering.contains(item.id) }

    var body: some View {
        @Bindable var model = model
        Form {
            Section {
                Text(item.title).font(.title3.weight(.semibold))
                Text(item.needed).foregroundStyle(.secondary)
            }
            if let content = item.content {
                Section("Details") {
                    Text(content).textSelection(.enabled)
                }
            }
            if let card = item.privileged {
                Section {
                    LabeledContent("Action", value: card.type ?? "Privileged action")
                    LabeledContent("Action id", value: card.actionID ?? "Missing")
                    LabeledContent("Digest", value: card.digestPrefix ?? "Missing").monospaced()
                } header: {
                    Text("Privileged action")
                } footer: {
                    Text("Approving needs Touch ID on this phone, which arrives with signed approvals. Until then, approve on the web app, or deny here.")
                }
            }
            if let expires = item.expiresAt {
                Section {
                    LabeledContent("Expires", value: expires.formatted(date: .abbreviated, time: .shortened))
                }
            }
            if item.isAnswerable {
                Section("Answer") {
                    ForEach(item.options, id: \.self) { option in
                        Button(option) { send(.option(option)) }
                            .disabled(busy || isUnsignedApprove(option))
                    }
                    TextField(item.privileged != nil ? "Deny with a note" : "Reply", text: reply, axis: .vertical)
                        .lineLimit(1...6)
                    Button(item.privileged != nil ? "Deny with note" : "Send reply") { send(.text(reply.wrappedValue)) }
                        .disabled(busy || reply.wrappedValue.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
                }
            }
        }
        .navigationTitle(item.kind.title)
        .navigationBarTitleDisplayMode(.inline)
        .overlay { if busy { ProgressView() } }
        .alert("Couldn't send", isPresented: Binding(get: { model.lastError != nil }, set: { if !$0 { model.lastError = nil } })) {
            Button("OK", role: .cancel) {}
        } message: {
            Text(model.lastError ?? "")
        }
    }

    private func isUnsignedApprove(_ option: String) -> Bool {
        item.privileged != nil && option == Answer.approve
    }

    private func send(_ answer: Answer) {
        Task { await model.answer(item, with: answer) }
    }
}

extension NeedsYouItem.Kind {
    var title: String {
        switch self {
        case .approval: "Approval"
        case .question: "Question"
        case .ownerDecision: "Decision"
        case .review: "Review"
        case .problem: "Problem"
        }
    }
}
