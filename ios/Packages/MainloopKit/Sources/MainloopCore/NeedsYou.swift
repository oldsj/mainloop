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
///
/// A card is privileged when its context names any `privileged_action_*` key, even if the
/// values are missing or malformed: such a card must never get an unsigned Approve. The facts
/// themselves are shown only when they are well-formed.
public struct PrivilegedCard: Hashable, Sendable {
    public static let keyPrefix = "privileged_action_"
    public static let contextActionID = "privileged_action_id"
    public static let contextDigest = "privileged_action_digest"
    public static let contextType = "privileged_action_type"

    public let actionID: String?
    /// SHA-256 of the frozen action, 64 lowercase hex characters; nil if the context's is not.
    public let digest: String?
    public let type: String?

    /// The first 16 characters, as the card shows them.
    public var digestPrefix: String? { digest.map { String($0.prefix(16)) } }

    /// The action id and digest an approval signs, when both are well formed. A card without them
    /// can't be approved here.
    public var approvable: (actionID: String, digest: String)? {
        guard let actionID, let digest else { return nil }
        return (actionID, digest)
    }

    public init?(context: [String: (any Sendable)?]) {
        guard context.keys.contains(where: { $0.hasPrefix(Self.keyPrefix) }) else { return nil }
        actionID = (context[Self.contextActionID] as? String).flatMap { $0.isEmpty ? nil : $0 }
        digest = (context[Self.contextDigest] as? String).flatMap {
            $0.count == 64 && $0.allSatisfy { $0.isHexDigit && !$0.isUppercase } ? $0 : nil
        }
        type = (context[Self.contextType] as? String).flatMap { $0.isEmpty ? nil : $0 }
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
    /// Approve on a privileged card, with the approval token signed by this phone's approval key
    /// (`ApprovalToken`).
    case signedApprove(token: String)

    public static let approve = "Approve"
    public static let deny = "Deny"

    public var response: String {
        switch self {
        case .option(let value), .text(let value): value
        case .signedApprove: Self.approve
        }
    }
}

public enum AnswerError: Error, Equatable, Sendable {
    case empty
    /// Approving a privileged action needs a signed approval; the phone never sends the web
    /// app's unsigned Approve, tapped or typed.
    case privilegedApproveNeedsSignature
    /// A signed approval for a card that isn't privileged, or whose action id or digest is
    /// missing or malformed.
    case notApprovable
}

/// Builds the body of `POST /queue/{item_id}/respond`.
public enum AnswerBody {
    public static func make(for item: NeedsYouItem, answer: Answer) throws -> Components.Schemas.QueueItemResponse {
        if case .signedApprove(let token) = answer {
            guard let facts = item.privileged?.approvable, !token.isEmpty else { throw AnswerError.notApprovable }
            // The server approves only when reviewed_context is the digest the approval signed.
            let metadata = try OpenAPIObjectContainer(unvalidatedValue: ["reviewed_context": facts.digest, "approval": token])
            return .init(response: Answer.approve, metadata: .init(additionalProperties: metadata))
        }
        let response = answer.response.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !response.isEmpty else { throw AnswerError.empty }
        // On a privileged card the server records anything but a signed Approve with the
        // card's digest as a denial. Refuse an unsigned Approve here, typed or tapped, so the
        // owner doesn't deny by accident.
        if item.privileged != nil, response.caseInsensitiveCompare(Answer.approve) == .orderedSame {
            throw AnswerError.privilegedApproveNeedsSignature
        }
        return .init(response: response)
    }
}
