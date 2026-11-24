import Foundation
import MainloopAPI
import OpenAPIRuntime

/// One row of Needs you (docs/specs/inbox.md), as the app shows it.
public struct NeedsYouItem: Identifiable, Hashable, Sendable {
    public enum Kind: String, Sendable {
        case approval, question, ownerDecision, review, problem
    }

    public let id: String
    public let kind: Kind
    public let title: String
    /// What the owner is asked for, in one line.
    public let needed: String
    /// The card's full text, when there is a queue item behind the row.
    public let content: String?
    /// Choices the card offers. Free text is always allowed too.
    public let options: [String]
    public let taskID: String?
    /// The id to answer with (`POST /queue/{item_id}/respond`); nil for rows that can't be
    /// answered here, such as problems.
    public let queueItemID: String?
    public let createdAt: Date?
    public let expiresAt: Date?
    public let privileged: PrivilegedCard?

    public var isAnswerable: Bool { queueItemID != nil && kind != .problem }
}

/// The privileged-action facts a card carries in its context (docs/specs/privileged-actions.md,
/// "Preflight and the card").
public struct PrivilegedCard: Hashable, Sendable {
    public static let contextActionID = "privileged_action_id"
    public static let contextDigest = "privileged_action_digest"
    public static let contextType = "privileged_action_type"

    public let actionID: String
    /// SHA-256 of the frozen action, 64 lowercase hex characters.
    public let digest: String
    public let type: String

    /// The first 16 characters, as the card shows them.
    public var digestPrefix: String { String(digest.prefix(16)) }

    public init?(context: [String: (any Sendable)?]) {
        guard let actionID = context[Self.contextActionID] as? String, !actionID.isEmpty,
              let digest = context[Self.contextDigest] as? String,
              digest.count == 64, digest.allSatisfy({ $0.isHexDigit && !$0.isUppercase })
        else { return nil }
        self.actionID = actionID
        self.digest = digest
        type = (context[Self.contextType] as? String) ?? ""
    }
}

extension NeedsYouItem.Kind {
    init(_ kind: Components.Schemas.AttentionItem.KindPayload) {
        switch kind {
        case .approval: self = .approval
        case .question: self = .question
        case .ownerDecision: self = .ownerDecision
        case .review: self = .review
        case .problem: self = .problem
        }
    }
}

extension NeedsYouItem {
    public init(_ item: Components.Schemas.AttentionItem) {
        let queue = item.queueItem
        id = item.id
        kind = Kind(item.kind)
        title = item.title
        needed = item.needed
        content = queue.map(\.content).flatMap { $0.isEmpty ? nil : $0 }
        options = queue?.options ?? []
        taskID = item.taskId ?? queue?.taskId
        queueItemID = queue?.id
        createdAt = queue?.createdAt
        expiresAt = queue?.expiresAt
        privileged = queue.flatMap { PrivilegedCard(context: $0.context.additionalProperties.value) }
    }
}

/// How the owner answers a card.
public enum Answer: Hashable, Sendable {
    /// One of the card's options, such as `Approve` or `Deny`.
    case option(String)
    /// A free-text reply. On a privileged card this is a denial with the text as its note.
    case text(String)

    public static let approve = "Approve"
    public static let deny = "Deny"

    public var response: String {
        switch self {
        case .option(let value), .text(let value): value
        }
    }
}

public enum AnswerError: Error, Equatable, Sendable {
    case empty
    /// Approving a privileged action needs a signed approval (slice 4); the phone can't send
    /// the web app's unsigned Approve.
    case privilegedApproveNeedsSignature
}

/// Builds the body of `POST /queue/{item_id}/respond`.
public enum AnswerBody {
    public static func make(for item: NeedsYouItem, answer: Answer) throws -> Components.Schemas.QueueItemResponse {
        let response = answer.response.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !response.isEmpty else { throw AnswerError.empty }
        if item.privileged != nil, answer == .option(Answer.approve) {
            throw AnswerError.privilegedApproveNeedsSignature
        }
        return .init(response: response)
    }
}
