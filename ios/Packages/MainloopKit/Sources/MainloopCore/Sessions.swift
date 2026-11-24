import Foundation
import MainloopAPI

/// One row of the sessions list.
public struct SessionRow: Identifiable, Hashable, Sendable {
    public enum Status: String, Sendable {
        case pending, active, idle, superseded, completed, failed, cancelled, waitingOnUser

        public var label: String {
            switch self {
            case .pending: "Starting"
            case .active: "Working"
            case .idle: "Idle"
            case .superseded: "Superseded"
            case .completed: "Completed"
            case .failed: "Failed"
            case .cancelled: "Cancelled"
            case .waitingOnUser: "Waiting on you"
            }
        }

        public var isLive: Bool { self == .pending || self == .active || self == .idle || self == .waitingOnUser }
    }

    public let id: String
    public let title: String
    public let summary: String?
    public let status: Status
    public let needsOwner: Bool
    public let agentKind: String?
    public let createdAt: Date
    public let isArchived: Bool

    public init(_ session: Components.Schemas.Session) {
        id = session.id
        title = session.title.isEmpty ? "Untitled session" : session.title
        summary = session.summary.flatMap { $0.isEmpty ? nil : $0 }
        status = Status(rawValue: Self.camel(session.status.rawValue)) ?? .idle
        needsOwner = session.needsOwner
        agentKind = session.agentKind
        createdAt = session.createdAt
        isArchived = session.archivedAt != nil
    }

    private static func camel(_ raw: String) -> String {
        let parts = raw.split(separator: "_")
        return parts.enumerated().map { $0.offset == 0 ? String($0.element) : $0.element.capitalized }.joined()
    }
}

extension Array where Element == SessionRow {
    /// Live sessions first (the ones that need the owner on top), then the rest, newest first.
    public func sortedForList() -> [SessionRow] {
        filter { !$0.isArchived }.sorted { a, b in
            if a.needsOwner != b.needsOwner { return a.needsOwner }
            if a.status.isLive != b.status.isLive { return a.status.isLive }
            return a.createdAt > b.createdAt
        }
    }
}
