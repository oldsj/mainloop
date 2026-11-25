import CryptoKit
import Foundation
import MainloopAPI
@testable import MainloopCore
import Testing

/// A privileged card like the fixture's first, with the given context.
func privilegedItem(_ context: [String: String]) throws -> NeedsYouItem {
    let base = try Fixtures.attention().items.map(NeedsYouItem.init)[0]
    return NeedsYouItem(
        id: base.id, kind: base.kind, title: base.title, needed: base.needed, content: nil,
        options: ["Approve", "Deny"], taskID: nil, queueItemID: base.queueItemID, createdAt: nil, expiresAt: nil,
        privileged: PrivilegedCard(context: context)
    )
}

/// An approver with a software key, or one whose prompt the owner cancels.
struct TestApprover: Approver {
    var canApprove = true
    var fails: (any Error)?
    let signer = SoftwareSigner()

    func approval(actionID: String, digest: String, reason: String) async throws -> String {
        if let fails { throw fails }
        return try await ApprovalToken.make(
            deviceID: deviceID, audience: "https://mainloop.example.test", actionID: actionID, digest: digest,
            signer: signer, reason: reason
        )
    }
}

let fixtureDigest = "3f2a9c1e7b6d4a5f8e0c2b1a9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f4a3b2c1d0e"

@Suite struct FixtureContractTests {
    @Test func attentionFixtureDecodesWithTheGeneratedTypes() throws {
        let snapshot = try Fixtures.attention()
        #expect(snapshot.items.count == 4)
        #expect(snapshot.count == 3)
        // Nullable fields the generator would otherwise drop (see PrepareContract).
        #expect(snapshot.items[0].queueItem?.id == "q-0001")
        #expect(snapshot.items[1].taskId == "task-0007")
        #expect(snapshot.items[3].queueItem == nil)
        #expect(snapshot.items[3].reasonCode == .stalled)
    }

    @Test func sessionsFixtureDecodes() throws {
        let sessions = try Fixtures.sessions()
        #expect(sessions.map(\.id) == ["sess-main-0001", "sess-sup-0002", "sess-task-0003", "sess-old-0004"])
        #expect(sessions[1].status == .waitingOnUser)
    }
}

@Suite struct NeedsYouItemTests {
    func items() throws -> [NeedsYouItem] { try Fixtures.attention().items.map(NeedsYouItem.init) }

    @Test func mapsAPrivilegedApproval() throws {
        let item = try items()[0]
        #expect(item.kind == .approval)
        #expect(item.queueItemID == "q-0001")
        #expect(item.options == ["Approve", "Deny"])
        let card = try #require(item.privileged)
        #expect(card.actionID == "pa-0001")
        #expect(card.type == "k8s.rollout_restart")
        #expect(card.digestPrefix == "3f2a9c1e7b6d4a5f")
        #expect(item.isAnswerable)
    }

    @Test func ordinaryCardsAreNotPrivileged() throws {
        let all = try items()
        #expect(all[1].privileged == nil)
        #expect(all[2].privileged == nil)
        #expect(all[1].taskID == "task-0007")
    }

    @Test func problemsAreNotAnswerable() throws {
        let problem = try items()[3]
        #expect(problem.kind == .problem)
        #expect(!problem.isAnswerable)
    }

    @Test(arguments: [
        ["privileged_action_id": "pa-1", "privileged_action_digest": "abc"],
        ["privileged_action_id": "pa-1", "privileged_action_digest": String(repeating: "A", count: 64)],
        ["privileged_action_id": "", "privileged_action_digest": String(repeating: "a", count: 64)],
        ["privileged_action_type": "k8s.delete"],
    ] as [[String: String]])
    func aMalformedPrivilegedContextIsStillPrivileged(_ context: [String: String]) throws {
        let card = try #require(PrivilegedCard(context: context))
        // Malformed facts are not shown as if they were real.
        #expect(card.digestPrefix == nil || card.actionID == nil)
    }

    @Test func aContextWithoutPrivilegedKeysIsOrdinary() {
        #expect(PrivilegedCard(context: ["repository": "example/lighthouse"]) == nil)
        #expect(PrivilegedCard(context: [:]) == nil)
    }
}

@Suite struct AnswerBodyTests {
    func items() throws -> [NeedsYouItem] { try Fixtures.attention().items.map(NeedsYouItem.init) }

    @Test func privilegedApproveIsRefusedUntilSigned() throws {
        let item = try items()[0]
        #expect(throws: AnswerError.privilegedApproveNeedsSignature) {
            try AnswerBody.make(for: item, answer: .option(Answer.approve))
        }
    }

    @Test(arguments: ["Approve", "approve", "  APPROVE \n"])
    func typedApproveOnAPrivilegedCardIsRefusedToo(_ text: String) throws {
        let item = try items()[0]
        #expect(throws: AnswerError.privilegedApproveNeedsSignature) {
            try AnswerBody.make(for: item, answer: .text(text))
        }
    }

    @Test func approveOnAPrivilegedCardWithAMalformedContextIsRefused() throws {
        let base = try items()[0]
        let item = NeedsYouItem(
            id: base.id, kind: base.kind, title: base.title, needed: base.needed, content: nil,
            options: ["Approve", "Deny"], taskID: nil, queueItemID: base.queueItemID, createdAt: nil, expiresAt: nil,
            privileged: PrivilegedCard(context: ["privileged_action_id": "pa-9"])
        )
        #expect(throws: AnswerError.privilegedApproveNeedsSignature) {
            try AnswerBody.make(for: item, answer: .option(Answer.approve))
        }
    }

    @Test func aSignedApproveCarriesTheDigestAndTheApproval() throws {
        let item = try items()[0]
        let body = try AnswerBody.make(for: item, answer: .signedApprove(token: "header.claims.signature"))
        let json = try JSONSerialization.jsonObject(with: JSONEncoder().encode(body)) as! [String: Any]
        #expect(json["response"] as? String == "Approve")
        let metadata = try #require(json["metadata"] as? [String: String])
        #expect(metadata == ["reviewed_context": fixtureDigest, "approval": "header.claims.signature"])
    }

    @Test(arguments: [
        ["privileged_action_id": "pa-9"],
        ["privileged_action_id": "pa-9", "privileged_action_digest": "abc"],
        ["privileged_action_digest": String(repeating: "a", count: 64)],
    ] as [[String: String]])
    func aSignedApproveNeedsWellFormedFacts(_ context: [String: String]) throws {
        #expect(throws: AnswerError.notApprovable) {
            try AnswerBody.make(for: privilegedItem(context), answer: .signedApprove(token: "t"))
        }
    }

    @Test func aSignedApproveIsOnlyForPrivilegedCards() throws {
        #expect(throws: AnswerError.notApprovable) {
            try AnswerBody.make(for: items()[2], answer: .signedApprove(token: "t"))
        }
    }

    @Test func ordinaryCardsMayBeApproved() throws {
        let merge = try items()[2]
        #expect(try AnswerBody.make(for: merge, answer: .text("Approve")).response == "Approve")
    }

    @Test func privilegedDenyAndNoteAreSent() throws {
        let item = try items()[0]
        #expect(try AnswerBody.make(for: item, answer: .option(Answer.deny)).response == "Deny")
        // Free text on a privileged card is a denial with that note (privileged-actions.md).
        #expect(try AnswerBody.make(for: item, answer: .text("  not today ")).response == "not today")
    }

    @Test func ordinaryOptionIsSentAsIs() throws {
        #expect(try AnswerBody.make(for: items()[1], answer: .option("ISO 8601")).response == "ISO 8601")
    }

    @Test func emptyReplyIsRefused() throws {
        #expect(throws: AnswerError.empty) { try AnswerBody.make(for: items()[1], answer: .text("   ")) }
    }
}

@MainActor @Suite struct ModelTests {
    @Test func answeringRemovesTheCardAndClearsTheSelection() async throws {
        let model = NeedsYouModel(source: try FixtureSource())
        await model.load()
        #expect(model.state == .loaded)
        #expect(model.count == 3)
        model.selection = "attn-0002"
        let item = try #require(model.selected)
        await model.answer(item, with: .option("ISO 8601"))
        #expect(model.items.map(\.id) == ["attn-0001", "attn-0003", "attn-0004"])
        #expect(model.selection == nil)
        #expect(model.lastError == nil)
    }

    @Test func privilegedApproveKeepsTheCardAndSaysWhy() async throws {
        let model = NeedsYouModel(source: try FixtureSource())
        await model.load()
        let item = try #require(model.items.first)
        await model.answer(item, with: .option(Answer.approve))
        #expect(model.items.count == 4)
        #expect(model.lastError?.contains("signed approval") == true)
    }

    @Test func approveIsOfferedOnlyWithAnApprovalKeyAndWellFormedFacts() async throws {
        let privileged = try Fixtures.attention().items.map(NeedsYouItem.init)[0]
        #expect(NeedsYouModel(source: try FixtureSource(), approver: TestApprover()).offersSignedApprove(privileged))
        // Registered before approval keys, or signed out.
        #expect(!NeedsYouModel(source: try FixtureSource(), approver: TestApprover(canApprove: false)).offersSignedApprove(privileged))
        #expect(!NeedsYouModel(source: try FixtureSource()).offersSignedApprove(privileged))
        // Malformed facts.
        let model = NeedsYouModel(source: try FixtureSource(), approver: TestApprover())
        #expect(!model.offersSignedApprove(try privilegedItem(["privileged_action_id": "pa-9", "privileged_action_digest": "abc"])))
        #expect(!model.offersSignedApprove(try privilegedItem(["privileged_action_type": "k8s.delete"])))
        // Ordinary cards answer with their options.
        #expect(!model.offersSignedApprove(try Fixtures.attention().items.map(NeedsYouItem.init)[2]))
    }

    @Test func approvingSignsAndRemovesTheCard() async throws {
        let model = NeedsYouModel(source: try FixtureSource(), approver: TestApprover())
        await model.load()
        let item = try #require(model.items.first)
        await model.approve(item)
        #expect(model.items.map(\.id) == ["attn-0002", "attn-0003", "attn-0004"])
        #expect(model.lastError == nil)
    }

    @Test func aCancelledPromptSendsNothingAndSaysNothing() async throws {
        let model = NeedsYouModel(source: try FixtureSource(), approver: TestApprover(fails: ApprovalSigningError.cancelled))
        await model.load()
        let item = try #require(model.items.first)
        await model.approve(item)
        #expect(model.items.count == 4)
        #expect(model.lastError == nil)
        #expect(model.answering.isEmpty)
    }

    @Test func anUnrecognisedFingerSendsNothing() async throws {
        let model = NeedsYouModel(source: try FixtureSource(), approver: TestApprover(fails: ApprovalSigningError.notRecognised))
        await model.load()
        await model.approve(try #require(model.items.first))
        #expect(model.items.count == 4)
        #expect(model.lastError?.contains("Nothing was sent") == true)
    }

    @Test func typedApproveIsStillRefusedWithAnApprovalKey() async throws {
        let model = NeedsYouModel(source: try FixtureSource(), approver: TestApprover())
        await model.load()
        let item = try #require(model.items.first)
        await model.answer(item, with: .text("approve"))
        await model.answer(item, with: .option(Answer.approve))
        #expect(model.items.count == 4)
        #expect(model.lastError != nil)
    }

    @Test func sessionsListLiveAndNeedsOwnerFirstWithoutArchived() async throws {
        let model = SessionsModel(source: try FixtureSource())
        await model.load()
        #expect(model.rows.map(\.id) == ["sess-sup-0002", "sess-main-0001", "sess-task-0003"])
        #expect(model.rows[0].status == .waitingOnUser)
        #expect(model.rows[0].status.label == "Waiting on you")
    }

    @Test func repliesBelongToTheirCard() async throws {
        let model = NeedsYouModel(source: try FixtureSource())
        await model.load()
        model.replies["attn-0001"] = "not on a Friday"
        model.replies["attn-0002"] = "either is fine"
        let question = try #require(model.items.first { $0.id == "attn-0002" })
        await model.answer(question, with: .text(model.replies["attn-0002"] ?? ""))
        #expect(model.replies["attn-0002"] == nil)
        #expect(model.replies["attn-0001"] == "not on a Friday")
    }

    @Test func draftsBelongToTheirSessionAndKeepTheirMessageIDForRetries() async throws {
        let source = try FixtureSource()
        let counter = Counter()
        let model = SessionsModel(source: source, newMessageID: { "m\(counter.next())" })
        model.setDraft("for main", for: "sess-main-0001")
        model.setDraft("for orchard", for: "sess-sup-0002")
        // Switching sessions (the split layout's selection) shows each session's own draft.
        #expect(model.draft(for: "sess-main-0001") == "for main")
        #expect(model.draft(for: "sess-sup-0002") == "for orchard")
        #expect(model.draft(for: "sess-task-0003") == "")
        model.setDraft("for orchard", for: "sess-sup-0002") // same text: same message id
        #expect(await model.sendDraft(to: "sess-sup-0002"))
        let sent = await source.sent
        #expect(sent.map(\.sessionID) == ["sess-sup-0002"])
        #expect(sent.map(\.message) == ["for orchard"])
        #expect(await source.sentIDs == ["m2"])
        #expect(model.draft(for: "sess-sup-0002") == "")
        #expect(model.draft(for: "sess-main-0001") == "for main")
    }

    @Test func sendTrimsAndSkipsEmpty() async throws {
        let source = try FixtureSource()
        let model = SessionsModel(source: source)
        #expect(await model.send("   ", to: "sess-main-0001", messageID: "m1") == false)
        #expect(await model.send(" hello ", to: "sess-main-0001", messageID: "m2"))
        let sent = await source.sent
        #expect(sent.map(\.message) == ["hello"])
    }
}

final class Counter: @unchecked Sendable {
    private let lock = NSLock()
    private var value = 0
    func next() -> Int { lock.withLock { value += 1; return value } }
}

@MainActor @Suite struct LiveApprovalTests {
    func model(_ transport: FakeTransport, approver: TestApprover) throws -> (NeedsYouModel, NeedsYouItem) {
        let client = MainloopAPI.client(origin: origin, middlewares: [], transport: transport)
        let item = try Fixtures.attention().items.map(NeedsYouItem.init)[0]
        return (NeedsYouModel(source: LiveSource(client: client), approver: approver), item)
    }

    @Test func approvingSendsTheSignedApproval() async throws {
        let transport = FakeTransport(json: #"{"status":"ok"}"#)
        let approver = TestApprover()
        let (model, item) = try model(transport, approver: approver)
        await model.approve(item)

        let sent = try #require(await transport.sent.first)
        #expect(sent.request.path == "/queue/q-0001/respond")
        let body = try JSONSerialization.jsonObject(with: try #require(sent.body)) as! [String: Any]
        #expect(body["response"] as? String == "Approve")
        let metadata = try #require(body["metadata"] as? [String: String])
        #expect(metadata["reviewed_context"] == fixtureDigest)
        let claims = try DecodedToken(try #require(metadata["approval"]), key: approver.signer.key.publicKey).claims
        #expect(claims["act"] as? String == "pa-0001")
        #expect(claims["dig"] as? String == fixtureDigest)
    }

    @Test func aRefusalShowsTheServersReason() async throws {
        let transport = FakeTransport(status: .forbidden, json: #"{"detail":"This device's approval key is not pinned for this action."}"#)
        let (model, item) = try model(transport, approver: TestApprover())
        await model.approve(item)
        #expect(model.lastError == "This device's approval key is not pinned for this action.")
    }

    @Test func aCancelledPromptSendsNoRequest() async throws {
        let transport = FakeTransport(json: #"{"status":"ok"}"#)
        let (model, item) = try model(transport, approver: TestApprover(fails: ApprovalSigningError.cancelled))
        await model.approve(item)
        #expect(await transport.sent.isEmpty)
    }
}
