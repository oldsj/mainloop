import Foundation
import MainloopAPI
import Observation

public enum LoadState: Equatable, Sendable {
    case idle, loading, loaded
    case failed(String)
}

@MainActor @Observable
public final class NeedsYouModel {
    public private(set) var items: [NeedsYouItem] = []
    public private(set) var state: LoadState = .idle
    /// The open card. On the inner screen it stays open through fold and unfold.
    public var selection: NeedsYouItem.ID?
    public private(set) var answering: Set<NeedsYouItem.ID> = []
    public var lastError: String?
    /// Free-text replies being written, per card. Kept here rather than in the detail view, so
    /// a note typed on one card can never be sent on another when the selection changes.
    public var replies: [NeedsYouItem.ID: String] = [:]

    private let source: any MainloopSource
    private let approver: (any Approver)?

    /// `approver` signs approvals of privileged cards; without one, privileged cards can't be
    /// approved here.
    public init(source: any MainloopSource, approver: (any Approver)? = nil) {
        self.source = source
        self.approver = approver
    }

    /// Whether this phone can sign approvals: it has an approval key.
    public var canSignApprovals: Bool { approver?.canApprove ?? false }

    /// Whether the card gets an Approve button: a privileged card with a well-formed action id
    /// and digest, on a phone that can sign approvals.
    public func offersSignedApprove(_ item: NeedsYouItem) -> Bool {
        item.isAnswerable && item.privileged?.approvable != nil && canSignApprovals
    }

    /// Decisions the owner can make here: everything but problems.
    public var count: Int { items.filter { $0.kind != .problem }.count }

    public var selected: NeedsYouItem? { items.first { $0.id == selection } }

    public func load() async {
        state = .loading
        do {
            items = try await source.needsYou()
            state = .loaded
            if let selection, !items.contains(where: { $0.id == selection }) { self.selection = nil }
        } catch {
            state = .failed(Self.describe(error))
        }
    }

    /// Sends the answer; the card leaves the list once the server takes it.
    public func answer(_ item: NeedsYouItem, with answer: Answer) async {
        answering.insert(item.id)
        defer { answering.remove(item.id) }
        await submit(item, answer)
    }

    /// Approves a privileged card: signs an approval for its action id and digest (Touch ID on a
    /// phone), then sends it. A cancelled prompt sends nothing and says nothing.
    public func approve(_ item: NeedsYouItem) async {
        guard let approver, offersSignedApprove(item), let facts = item.privileged?.approvable else {
            lastError = Self.describe(AnswerError.notApprovable)
            return
        }
        answering.insert(item.id)
        defer { answering.remove(item.id) }
        let token: String
        do {
            token = try await approver.approval(actionID: facts.actionID, digest: facts.digest, reason: "Approve: \(item.title)")
        } catch ApprovalSigningError.cancelled {
            return
        } catch {
            lastError = Self.describe(error)
            return
        }
        await submit(item, .signedApprove(token: token))
    }

    private func submit(_ item: NeedsYouItem, _ answer: Answer) async {
        do {
            try await source.respond(to: item, with: answer)
            remove(item.id)
        } catch SourceError.alreadyAnswered {
            remove(item.id)
            lastError = "Already answered."
        } catch {
            lastError = Self.describe(error)
        }
    }

    private func remove(_ id: NeedsYouItem.ID) {
        items.removeAll { $0.id == id }
        replies[id] = nil
        if selection == id { selection = nil }
    }

    static func describe(_ error: any Error) -> String {
        switch error {
        case SourceError.signedOut: "Signed out. Sign in from Settings."
        case SourceError.alreadyAnswered: "Already answered."
        case SourceError.notAnswerable: "This item can't be answered here."
        case SourceError.unexpectedStatus(let status): "The server answered \(status)."
        case SourceError.refused(let message): message
        case AnswerError.empty: "Write a reply first."
        case AnswerError.privilegedApproveNeedsSignature:
            "Approving a privileged action needs a signed approval: use Approve, which asks for Touch ID."
        case AnswerError.notApprovable: "This card can't be approved here."
        case ApprovalError.noApprovalKey: "Sign out and in again to approve privileged actions on this iPhone."
        case ApprovalSigningError.notRecognised: "Touch ID didn't recognise you. Nothing was sent."
        case ApprovalSigningError.biometryUnavailable:
            "Touch ID isn't available. Nothing was sent. Check Touch ID & Passcode in Settings."
        case ApprovalSigningError.keyUnusable:
            "This iPhone's approval key no longer works, as happens when Touch ID fingers change. Sign out and in again."
        default: error.localizedDescription
        }
    }
}

@MainActor @Observable
public final class SessionsModel {
    public private(set) var rows: [SessionRow] = []
    public private(set) var state: LoadState = .idle
    public var selection: SessionRow.ID?
    public var lastError: String?
    public private(set) var sending = false
    /// Messages being written, per session, each with the `message_id` it will be sent with.
    private var drafts: [SessionRow.ID: Draft] = [:]

    struct Draft {
        var text: String
        var messageID: String
    }

    private let source: any MainloopSource
    private let newMessageID: @Sendable () -> String

    public init(source: any MainloopSource, newMessageID: @escaping @Sendable () -> String = { UUID().uuidString }) {
        self.source = source
        self.newMessageID = newMessageID
    }

    public var selected: SessionRow? { rows.first { $0.id == selection } }

    public func draft(for sessionID: SessionRow.ID) -> String { drafts[sessionID]?.text ?? "" }

    /// Changing the text gives the draft a new `message_id`; the same text keeps its id, so a
    /// retry after a failed send is one message on the server.
    public func setDraft(_ text: String, for sessionID: SessionRow.ID) {
        if drafts[sessionID]?.text == text { return }
        drafts[sessionID] = Draft(text: text, messageID: newMessageID())
    }

    /// Sends the session's draft and clears it on success.
    @discardableResult
    public func sendDraft(to sessionID: SessionRow.ID) async -> Bool {
        guard let draft = drafts[sessionID] else { return false }
        let sent = await send(draft.text, to: sessionID, messageID: draft.messageID)
        if sent, drafts[sessionID]?.messageID == draft.messageID { drafts[sessionID] = nil }
        return sent
    }

    public func load() async {
        state = .loading
        do {
            rows = try await source.sessions().sortedForList()
            state = .loaded
        } catch {
            state = .failed(NeedsYouModel.describe(error))
        }
    }

    /// Sends one message. `messageID` makes a retry of the same message idempotent on the
    /// server, so the caller keeps it until the send succeeds.
    @discardableResult
    public func send(_ text: String, to sessionID: String, messageID: String) async -> Bool {
        let message = text.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !message.isEmpty else { return false }
        sending = true
        defer { sending = false }
        do {
            try await source.send(message, to: sessionID, messageID: messageID)
            return true
        } catch {
            lastError = NeedsYouModel.describe(error)
            return false
        }
    }
}
