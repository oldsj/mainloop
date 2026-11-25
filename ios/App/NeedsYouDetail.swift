import MainloopCore
import SwiftUI

/// One card: what is asked, the facts, and the answers. A privileged card shows the action id
/// and the digest prefix as the web card does. Its Approve opens the approval sheet on the
/// action's record, which this phone fetched and hashed itself; the sheet's Approve signs that
/// record's digest with the approval key, which asks for Touch ID. Its other answers are denials.
struct NeedsYouDetail: View {
    @Environment(NeedsYouModel.self) private var model
    let item: NeedsYouItem

    /// The note belongs to this card in the model, never to whichever card is open.
    private var reply: Binding<String> {
        Binding(get: { model.replies[item.id] ?? "" }, set: { model.replies[item.id] = $0 })
    }

    private var busy: Bool { model.answering.contains(item.id) }

    /// The approval sheet, for this card only.
    private var review: Binding<ApprovalReview?> {
        Binding(
            get: { model.reviewing?.item.id == item.id ? model.reviewing : nil },
            set: { if $0 == nil { model.reviewing = nil } }
        )
    }

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
                            Task { await model.review(item) }
                        } label: {
                            Label("Review and approve", systemImage: "touchid")
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
        .sheet(item: review) { ApprovalSheet(review: $0) }
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
            return "Review and approve shows the action as this iPhone reads it from the server's record and checks it against this card. Approving there asks for Touch ID and signs that record's digest with a key that stays in this iPhone's Secure Enclave."
        }
        return "Sign out and in again to approve privileged actions on this iPhone."
    }

    private func send(_ answer: Answer) {
        Task { await model.answer(item, with: answer) }
    }
}

/// What Touch ID signs, shown before it asks: the action's canonical record as this phone parsed
/// it, never the card's text. Hidden characters are shown as ⟨U+XXXX⟩.
struct ApprovalSheet: View {
    @Environment(NeedsYouModel.self) private var model
    let review: ApprovalReview

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    Text(review.record.type).font(.title3.weight(.semibold)).monospaced()
                } footer: {
                    Text("This is the action Touch ID approves, read from the record this iPhone fetched and hashed itself.")
                }
                Section("Arguments") {
                    ForEach(review.record.args, id: \.self) { FieldRow(field: $0) }
                }
                if !review.record.facts.isEmpty {
                    Section("What preflight read") {
                        ForEach(review.record.facts, id: \.self) { FieldRow(field: $0) }
                    }
                }
                Section("Request") {
                    ForEach(review.record.context, id: \.self) { FieldRow(field: $0) }
                }
            }
            .navigationTitle("Approve")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") { model.reviewing = nil }
                }
                ToolbarItem(placement: .confirmationAction) {
                    Button {
                        Task { await model.confirm(review) }
                    } label: {
                        Label(Answer.approve, systemImage: "touchid")
                    }
                    .disabled(model.answering.contains(review.id))
                }
            }
        }
    }
}

private struct FieldRow: View {
    let field: ActionRecord.Field

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            Text(field.label).font(.caption).foregroundStyle(.secondary)
            Text(field.value).font(.callout.monospaced()).textSelection(.enabled)
        }
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
