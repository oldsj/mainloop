import MainloopCore
import SwiftUI

/// One card: what is asked, the facts, and the answers. A privileged card shows the action id
/// and the digest prefix as the web card does. Its Approve signs an approval with this phone's
/// approval key, which asks for Touch ID; its other answers are denials.
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
                    Text(privilegedFooter(card))
                }
            }
            if let expires = item.expiresAt {
                Section {
                    LabeledContent("Expires", value: expires.formatted(date: .abbreviated, time: .shortened))
                }
            }
            if item.isAnswerable {
                Section("Answer") {
                    if model.offersSignedApprove(item) {
                        Button {
                            Task { await model.approve(item) }
                        } label: {
                            Label(Answer.approve, systemImage: "touchid")
                        }
                        .disabled(busy)
                    }
                    // A privileged card's Approve is the signed one above, never the plain option.
                    ForEach(item.options.filter { !isUnsignedApprove($0) }, id: \.self) { option in
                        Button(option) { send(.option(option)) }
                            .disabled(busy)
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
        item.privileged != nil && option.caseInsensitiveCompare(Answer.approve) == .orderedSame
    }

    private func privilegedFooter(_ card: PrivilegedCard) -> String {
        if card.approvable == nil {
            return "This card's action id or digest is missing or malformed, so it can't be approved. Deny it, or ask the agent again."
        }
        if model.canSignApprovals {
            return "Approve asks for Touch ID, then signs this action id and digest with a key that stays in this iPhone's Secure Enclave."
        }
        return "Sign out and in again to approve privileged actions on this iPhone."
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
