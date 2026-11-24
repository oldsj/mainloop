import Foundation
import MainloopAPI

/// Where the app's data comes from: the live API, or fictional fixtures (previews, the
/// simulator, and any build without a real origin).
public protocol MainloopSource: Sendable {
    func needsYou() async throws -> [NeedsYouItem]
    func respond(to item: NeedsYouItem, with answer: Answer) async throws
    func sessions() async throws -> [SessionRow]
    func send(_ message: String, to sessionID: String, messageID: String) async throws
}

public enum SourceError: Error, Equatable, Sendable {
    case notAnswerable
    case unexpectedStatus(Int)
    /// A 401: the sign-in lapsed or there is none yet.
    case signedOut
    case alreadyAnswered
}

/// The generated client against one Mainloop origin. The app adds `AccountSigningMiddleware`, so
/// a registered phone signs every call (docs/specs/devices.md); an unregistered one gets 401.
public struct LiveSource: MainloopSource {
    let client: Client

    public init(client: Client) { self.client = client }

    public func needsYou() async throws -> [NeedsYouItem] {
        switch try await client.getAttention() {
        case .ok(let ok): return try ok.body.json.items.map(NeedsYouItem.init)
        case .unauthorized: throw SourceError.signedOut
        case .forbidden: throw SourceError.unexpectedStatus(403)
        case .notFound: throw SourceError.unexpectedStatus(404)
        case .undocumented(let status, _): throw SourceError.unexpectedStatus(status)
        }
    }

    public func respond(to item: NeedsYouItem, with answer: Answer) async throws {
        guard item.isAnswerable, let queueItemID = item.queueItemID else { throw SourceError.notAnswerable }
        let body = try AnswerBody.make(for: item, answer: answer)
        switch try await client.respondToQueueItem(path: .init(itemId: queueItemID), body: .json(body)) {
        case .ok: return
        case .unauthorized: throw SourceError.signedOut
        case .conflict: throw SourceError.alreadyAnswered
        case .forbidden: throw SourceError.unexpectedStatus(403)
        case .notFound: throw SourceError.unexpectedStatus(404)
        case .contentTooLarge: throw SourceError.unexpectedStatus(413)
        case .unprocessableContent: throw SourceError.unexpectedStatus(422)
        case .undocumented(let status, _): throw SourceError.unexpectedStatus(status)
        }
    }

    public func sessions() async throws -> [SessionRow] {
        switch try await client.listSessions() {
        case .ok(let ok): return try ok.body.json.map(SessionRow.init)
        case .unauthorized: throw SourceError.signedOut
        case .undocumented(let status, _): throw SourceError.unexpectedStatus(status)
        default: throw SourceError.unexpectedStatus(0)
        }
    }

    public func send(_ message: String, to sessionID: String, messageID: String) async throws {
        let body = Components.Schemas.SessionMessageRequest(message: message, messageId: messageID)
        switch try await client.sendSessionMessage(path: .init(sessionId: sessionID), body: .json(body)) {
        case .ok, .accepted: return
        case .unauthorized: throw SourceError.signedOut
        case .undocumented(let status, _): throw SourceError.unexpectedStatus(status)
        default: throw SourceError.unexpectedStatus(0)
        }
    }
}

/// Fictional data from `Fixtures/`, decoded with the generated types so a fixture that drifts
/// from the contract fails its test. Answers and messages change only this copy.
public actor FixtureSource: MainloopSource {
    private var items: [NeedsYouItem]
    private var rows: [SessionRow]
    public private(set) var sent: [(sessionID: String, message: String)] = []
    public private(set) var sentIDs: [String] = []

    public init() throws {
        items = try Fixtures.attention().items.map(NeedsYouItem.init)
        rows = try Fixtures.sessions().map(SessionRow.init)
    }

    public func needsYou() async throws -> [NeedsYouItem] { items }

    public func respond(to item: NeedsYouItem, with answer: Answer) async throws {
        guard item.isAnswerable else { throw SourceError.notAnswerable }
        _ = try AnswerBody.make(for: item, answer: answer)
        guard items.contains(where: { $0.id == item.id }) else { throw SourceError.alreadyAnswered }
        items.removeAll { $0.id == item.id }
    }

    public func sessions() async throws -> [SessionRow] { rows }

    public func send(_ message: String, to sessionID: String, messageID: String) async throws {
        sent.append((sessionID, message))
        sentIDs.append(messageID)
    }
}

public enum Fixtures {
    static func decoder() -> JSONDecoder {
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .iso8601
        return decoder
    }

    static func data(_ name: String) throws -> Data {
        guard let url = Bundle.module.url(forResource: name, withExtension: "json", subdirectory: "Fixtures") else {
            throw CocoaError(.fileNoSuchFile)
        }
        return try Data(contentsOf: url)
    }

    public static func attention() throws -> Components.Schemas.AttentionSnapshot {
        try decoder().decode(Components.Schemas.AttentionSnapshot.self, from: data("attention"))
    }

    public static func sessions() throws -> [Components.Schemas.Session] {
        try decoder().decode([Components.Schemas.Session].self, from: data("sessions"))
    }
}
