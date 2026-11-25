import Foundation
import MainloopAPI
import Observation
import OpenAPIRuntime

/// What the web app asks the phone to approve (docs/specs/ios-app.md, "Approving in the app"): a
/// privileged-action card, by its queue item id and action id.
///
/// The ids only name what to fetch. The phone reads the action's canonical record itself, hashes
/// it, shows it, and signs that digest; whatever digest the page sent is never used. The server
/// accepts the signed approval only for the card whose own action and digest it names, so a page
/// that pairs a card with another action gets a refusal, not an approval.
public struct ApprovalRequest: Hashable, Sendable {
    public let queueItemID: String
    public let actionID: String

    public init?(queueItemID: String, actionID: String) {
        guard Self.isIdentifier(queueItemID), Self.isIdentifier(actionID) else { return nil }
        self.queueItemID = queueItemID
        self.actionID = actionID
    }

    /// Non-empty, at most 200 visible ASCII characters.
    static func isIdentifier(_ id: String) -> Bool {
        !id.isEmpty && id.utf8.count <= 200 && id.unicodeScalars.allSatisfy { $0.value > 0x20 && $0.value < 0x7F }
    }
}

/// An approval being reviewed: what was asked, and the record the sheet shows and Touch ID signs.
public struct ApprovalReview: Identifiable, Hashable, Sendable {
    public let request: ApprovalRequest
    public let record: ActionRecord
    public var id: String { request.queueItemID }
}

/// How an approval ended, as the app answers the page.
public enum ApprovalOutcome: Equatable, Sendable {
    case approved
    /// The owner closed the sheet. Nothing was signed or sent.
    case cancelled
    /// Nothing was approved, for this reason.
    case failed(String)

    /// The reply to the page's `approve` message (web/src/lib/nativeShell.ts).
    public var reply: [String: String] {
        switch self {
        case .approved: ["status": "approved"]
        case .cancelled: ["status": "cancelled"]
        case .failed(let message): ["status": "failed", "message": message]
        }
    }
}

/// The two calls an approval makes.
public protocol ApprovalService: Sendable {
    /// The action's canonical record (`GET /privileged-actions/{id}`, `canonical_record`).
    func record(_ actionID: String) async throws -> String
    /// Approve on the card, with the approval token and the digest it signed.
    func approve(queueItemID: String, token: String, digest: String) async throws
}

public enum ApprovalServiceError: Error, Equatable, Sendable {
    /// A 401: this phone isn't recognised.
    case signedOut
    case alreadyAnswered
    /// A 403 with the server's reason, such as an approval it didn't accept.
    case refused(String)
    case unexpectedStatus(Int)
}

/// The generated client, signed by the device key (`AccountSigningMiddleware`).
public struct LiveApprovalService: ApprovalService {
    let client: any APIProtocol

    public init(client: any APIProtocol) { self.client = client }

    public func record(_ actionID: String) async throws -> String {
        switch try await client.getPrivilegedAction(path: .init(actionId: actionID)) {
        case .ok(let ok):
            guard let record = try ok.body.json.canonicalRecord else { throw ActionRecordError.malformed }
            return record
        case .unauthorized: throw ApprovalServiceError.signedOut
        case .undocumented(let status, _): throw ApprovalServiceError.unexpectedStatus(status)
        default: throw ApprovalServiceError.unexpectedStatus(0)
        }
    }

    public func approve(queueItemID: String, token: String, digest: String) async throws {
        // The server approves only when reviewed_context is the digest the approval signed.
        let metadata = try OpenAPIObjectContainer(unvalidatedValue: ["reviewed_context": digest, "approval": token])
        let body = Components.Schemas.QueueItemResponse(response: "Approve", metadata: .init(additionalProperties: metadata))
        switch try await client.respondToQueueItem(path: .init(itemId: queueItemID), body: .json(body)) {
        case .ok: return
        case .unauthorized: throw ApprovalServiceError.signedOut
        case .conflict: throw ApprovalServiceError.alreadyAnswered
        case .forbidden(let forbidden):
            if let detail = try? forbidden.body.json.detail, !detail.isEmpty { throw ApprovalServiceError.refused(detail) }
            throw ApprovalServiceError.unexpectedStatus(403)
        case .notFound: throw ApprovalServiceError.unexpectedStatus(404)
        case .contentTooLarge: throw ApprovalServiceError.unexpectedStatus(413)
        case .unprocessableContent: throw ApprovalServiceError.unexpectedStatus(422)
        case .undocumented(let status, _): throw ApprovalServiceError.unexpectedStatus(status)
        }
    }
}

/// The native approval sheet's state. The page asks with `request`, which answers once the owner
/// approves, cancels, or it fails; one approval at a time.
@MainActor @Observable
public final class ApprovalModel {
    /// The open sheet.
    public private(set) var reviewing: ApprovalReview?
    /// Touch ID is up, or the answer is being sent.
    public private(set) var signing = false

    private let service: (any ApprovalService)?
    private let approver: (any Approver)?
    private var active = false
    private var pending: CheckedContinuation<ApprovalOutcome, Never>?

    /// Without a service and an approver (not paired), every request fails and says why.
    public init(service: (any ApprovalService)?, approver: (any Approver)?) {
        self.service = service
        self.approver = approver
    }

    public static let notPaired = "Pair this iPhone to approve privileged actions. Nothing was approved."
    public static let busy = "Another approval is open on this iPhone. Nothing was approved."

    /// Fetches and hashes the action's record, opens the sheet on it and waits for the owner.
    public func request(_ request: ApprovalRequest) async -> ApprovalOutcome {
        guard !active else { return .failed(Self.busy) }
        guard let service, let approver, approver.canApprove else { return .failed(Self.notPaired) }
        active = true
        let record: ActionRecord
        do {
            record = try ActionRecord.read(try await service.record(request.actionID), actionID: request.actionID)
        } catch {
            active = false
            return .failed(Self.describe(error))
        }
        return await withCheckedContinuation { continuation in
            pending = continuation
            reviewing = ApprovalReview(request: request, record: record)
        }
    }

    /// The sheet's Cancel.
    public func cancel() {
        guard !signing else { return }
        finish(.cancelled)
    }

    /// The sheet's Approve: signs the record's own action id and digest (Touch ID), then sends it.
    /// A cancelled Touch ID prompt sends nothing and keeps the sheet open.
    public func confirm() async {
        guard let review = reviewing, let service, let approver, !signing else { return }
        signing = true
        defer { signing = false }
        let token: String
        do {
            token = try await approver.approval(actionID: review.record.actionID, digest: review.record.digest, reason: review.record.reason)
        } catch ApprovalSigningError.cancelled {
            return
        } catch {
            finish(.failed(Self.describe(error)))
            return
        }
        do {
            try await service.approve(queueItemID: review.request.queueItemID, token: token, digest: review.record.digest)
            finish(.approved)
        } catch {
            finish(.failed(Self.describe(error)))
        }
    }

    private func finish(_ outcome: ApprovalOutcome) {
        reviewing = nil
        active = false
        let continuation = pending
        pending = nil
        continuation?.resume(returning: outcome)
    }

    static func describe(_ error: any Error) -> String {
        switch error {
        case ApprovalServiceError.signedOut: "Mainloop didn't recognise this iPhone. Nothing was approved."
        case ApprovalServiceError.alreadyAnswered: "This card was already answered."
        case ApprovalServiceError.refused(let message): message
        case ApprovalServiceError.unexpectedStatus(let status): "Mainloop answered \(status). Nothing was approved."
        case ActionRecordError.otherAction:
            "The server's record names another action than this card, so nothing was signed."
        case ActionRecordError.malformed:
            "This action's record couldn't be read, so nothing was signed."
        case ApprovalError.noApprovalKey: "Sign out and in again to approve privileged actions on this iPhone."
        case ApprovalSigningError.notRecognised: "Touch ID didn't recognise you. Nothing was sent."
        case ApprovalSigningError.biometryUnavailable:
            "Touch ID isn't available. Nothing was sent. Check Touch ID & Passcode in Settings."
        case ApprovalSigningError.keyUnusable:
            "This iPhone's approval key no longer works, as happens when Touch ID fingers change. Sign out and in again."
        case is URLError: "Couldn't reach Mainloop. Nothing was approved."
        default: "Nothing was approved: \(error.localizedDescription)"
        }
    }
}

/// The app's bundled fictional data, for tests and previews.
public enum Fixtures {
    static func data(_ name: String) throws -> Data {
        guard let url = Bundle.module.url(forResource: name, withExtension: "json", subdirectory: "Fixtures") else {
            throw CocoaError(.fileNoSuchFile)
        }
        return try Data(contentsOf: url)
    }

    /// A privileged action's canonical record, a copy of the Go vector
    /// (server/internal/privileged/testdata/canonical-record.json).
    public static func privilegedRecord() throws -> (record: String, digest: String) {
        struct Vector: Decodable { let record: String; let digest: String }
        let vector = try JSONDecoder().decode(Vector.self, from: data("privileged-record"))
        return (vector.record, vector.digest)
    }
}
