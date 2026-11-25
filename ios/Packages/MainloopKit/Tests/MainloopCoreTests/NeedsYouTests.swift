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

let fixtureDigest = "8a6714a445634201211bad2c2b296d2d521c69bfd33488e9484ae2ced57e316e"

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
        #expect(card.digestPrefix == "8a6714a445634201")
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
        let body = try AnswerBody.make(for: item, answer: .signedApprove(token: "header.claims.signature", digest: fixtureDigest))
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
            try AnswerBody.make(for: privilegedItem(context), answer: .signedApprove(token: "t", digest: fixtureDigest))
        }
    }

    @Test func aSignedApproveMustSignTheCardsDigest() throws {
        #expect(throws: AnswerError.notApprovable) {
            try AnswerBody.make(for: items()[0], answer: .signedApprove(token: "t", digest: String(repeating: "b", count: 64)))
        }
    }

    @Test func aSignedApproveIsOnlyForPrivilegedCards() throws {
        #expect(throws: AnswerError.notApprovable) {
            try AnswerBody.make(for: items()[2], answer: .signedApprove(token: "t", digest: fixtureDigest))
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
        await model.reviewAndConfirm(item)
        #expect(model.items.map(\.id) == ["attn-0002", "attn-0003", "attn-0004"])
        #expect(model.lastError == nil)
    }

    @Test func aCancelledPromptSendsNothingAndSaysNothing() async throws {
        let model = NeedsYouModel(source: try FixtureSource(), approver: TestApprover(fails: ApprovalSigningError.cancelled))
        await model.load()
        let item = try #require(model.items.first)
        await model.reviewAndConfirm(item)
        #expect(model.items.count == 4)
        #expect(model.lastError == nil)
        #expect(model.answering.isEmpty)
    }

    @Test func anUnrecognisedFingerSendsNothing() async throws {
        let model = NeedsYouModel(source: try FixtureSource(), approver: TestApprover(fails: ApprovalSigningError.notRecognised))
        await model.load()
        await model.reviewAndConfirm(try #require(model.items.first))
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

extension NeedsYouModel {
    /// Approve as the sheet does it: review the record, then confirm what it showed.
    func reviewAndConfirm(_ item: NeedsYouItem) async {
        await review(item)
        if let reviewing { await confirm(reviewing) }
    }
}

/// `GET /privileged-actions/{id}`'s answer, with `record` as its canonical record.
func actionDetailJSON(_ record: String) throws -> String {
    let detail: [String: Any] = [
        "action": [
            "id": "pa-0001", "type": "k8s.rollout_restart", "state": "pending_approval", "title": "Restart web",
            "requester_role": "worker", "requester_session_id": "sess-0001", "requester_task_id": "task-0001",
            "reason": "A fictional reason.", "created_at": "2026-11-24T23:00:00Z", "expires_at": "2026-11-24T23:30:00Z",
            "decided_at": NSNull(), "finished_at": NSNull(), "decision_note": NSNull(), "last_error": NSNull(),
        ] as [String: Any],
        "body": "A card the server could have written to say anything.", "args": [:] as [String: Any],
        "frozen_facts": NSNull(), "digest": NSNull(), "canonical_record": record, "result": NSNull(), "attempts": 0,
        "delivery_state": "none", "events": [] as [Any], "approval": NSNull(),
    ]
    return String(decoding: try JSONSerialization.data(withJSONObject: detail), as: UTF8.self)
}

@MainActor @Suite struct ReviewTests {
    @Test func theSheetShowsTheRecordThePhoneHashed() async throws {
        let model = NeedsYouModel(source: try FixtureSource(), approver: TestApprover())
        await model.load()
        let item = try #require(model.items.first)
        await model.review(item)
        let review = try #require(model.reviewing)
        #expect(review.record.digest == fixtureDigest)
        #expect(review.record.type == "k8s.rollout_restart")
        #expect(review.record.args.map(\.label) == ["kind", "name", "namespace"])
        #expect(review.record.args.first { $0.label == "namespace" }?.value == "shop")
        // A bidi override is shown, never applied.
        let note = try #require(review.record.facts.first { $0.label == "note" }?.value)
        #expect(note.contains("⟨U+202E⟩evil"))
        #expect(!note.unicodeScalars.contains("\u{202E}"))
        #expect(review.record.reason == "Approve k8s.rollout_restart (pa-0001)")
        // Nothing is sent until the owner confirms.
        #expect(model.items.count == 4)
    }

    @Test func aRecordThatIsNotTheCardsIsRefusedAndNothingIsSigned() async throws {
        let (record, _) = try Fixtures.privilegedRecord()
        let cases: [(String, ActionRecordError)] = [
            // A server that shows a routine card but serves another action's record.
            (record.replacingOccurrences(of: "\"web\"", with: "\"payments\""), .digestMismatch),
        ]
        for (served, error) in cases {
            let transport = FakeTransport(json: #"{"status":"ok"}"#, routes: ["getPrivilegedAction": try actionDetailJSON(served)])
            let client = MainloopAPI.client(origin: origin, middlewares: [], transport: transport)
            let model = NeedsYouModel(source: LiveSource(client: client), approver: TestApprover())
            await model.reviewAndConfirm(try Fixtures.attention().items.map(NeedsYouItem.init)[0])
            #expect(model.reviewing == nil)
            #expect(model.lastError == NeedsYouModel.describe(error))
            #expect(await transport.sent.allSatisfy { $0.request.method == .get })
        }
    }

    @Test func aRecordForAnotherActionWithTheCardsDigestIsRefused() throws {
        // A card whose id the server swapped, with the digest of the real record: the record names
        // pa-0001, the card pa-0002.
        let card = try #require(PrivilegedCard(context: [
            "privileged_action_id": "pa-0002", "privileged_action_digest": fixtureDigest, "privileged_action_type": "k8s.rollout_restart",
        ]))
        #expect(throws: ActionRecordError.otherAction) { try ActionRecord.verify(try Fixtures.privilegedRecord().record, for: card) }
        let typed = try #require(PrivilegedCard(context: [
            "privileged_action_id": "pa-0001", "privileged_action_digest": fixtureDigest, "privileged_action_type": "k8s.delete",
        ]))
        #expect(throws: ActionRecordError.otherAction) { try ActionRecord.verify(try Fixtures.privilegedRecord().record, for: typed) }
    }

    @Test func aCancelledPromptKeepsTheSheet() async throws {
        let model = NeedsYouModel(source: try FixtureSource(), approver: TestApprover(fails: ApprovalSigningError.cancelled))
        await model.load()
        await model.review(try #require(model.items.first))
        await model.confirm(try #require(model.reviewing))
        #expect(model.reviewing != nil)
        #expect(model.items.count == 4)
    }
}

/// The cross-language vector: the canonical record the Go server computes
/// (server/internal/privileged/testdata/canonical-record.json, from TestTheCanonicalRecordVector),
/// hashed here to the same digest. The app's fixture is a copy of it.
@Suite struct ActionRecordTests {
    static let goVector = URL(fileURLWithPath: #filePath)
        .deletingLastPathComponent().appendingPathComponent("../../../../../server/internal/privileged/testdata/canonical-record.json")
        .standardizedFileURL

    struct Vector: Decodable, Equatable { let record: String; let digest: String }

    @Test func theGoDigestIsTheSwiftDigest() throws {
        let vector = try JSONDecoder().decode(Vector.self, from: Data(contentsOf: Self.goVector))
        let card = try #require(PrivilegedCard(context: [
            "privileged_action_id": "pa-0001", "privileged_action_digest": vector.digest, "privileged_action_type": "k8s.rollout_restart",
        ]))
        let record = try ActionRecord.verify(vector.record, for: card)
        #expect(record.digest == vector.digest)
        #expect(record.facts.first { $0.label == "ratio" }?.value == "1.5")
        #expect(record.facts.first { $0.label == "image" }?.value == "registry.example.test/shop/web:1.50")
    }

    @Test func theAppFixtureIsTheGoVector() throws {
        let go = try JSONDecoder().decode(Vector.self, from: Data(contentsOf: Self.goVector))
        let (record, digest) = try Fixtures.privilegedRecord()
        #expect(Vector(record: record, digest: digest) == go)
    }

    @Test func argvIsQuotedAsTheWebCardQuotesIt() {
        #expect(ActionRecord.shellQuote(["kubectl", "get", "pods", "-l", "app=web"]) == "kubectl get pods -l app=web")
        // Two words never read as one, and a quote can't end the word early.
        #expect(ActionRecord.shellQuote(["sh", "-c", "rm -rf /tmp/x", "it's", ""]) == #"sh -c 'rm -rf /tmp/x' 'it'\''s' ''"#)
        let fields = ActionRecord.fields(["argv": ["talosctl", "upgrade", "--image", "a b"]])
        #expect(fields == [.init(label: "argv", value: "talosctl upgrade --image 'a b'", kind: .command)])
    }

    @Test func aDiffInTheFactsGetsItsOwnBlock() {
        let diff = "--- live\n+++ after\n@@ -1,2 +1,2 @@\n-replicas: 2\n+replicas: 3\n"
        let fields = ActionRecord.fields([
            "changes": [["name": "web", "diff": diff], ["name": "api", "note": "a\nb"]],
            "cluster": "lab-01",
            "preview": "line one\nline two",
        ])
        #expect(fields.map(\.label) == ["changes", "changes[0].diff", "changes[1].note", "cluster", "preview"])
        #expect(fields[0].value.contains(#""diff" : "⟨below: changes[0].diff⟩""#))
        #expect(fields[0].value.contains(#""note" : "⟨below: changes[1].note⟩""#))
        #expect(fields[1] == .init(label: "changes[0].diff", value: diff, kind: .diff))
        #expect(fields[2].kind == .lines)
        #expect(fields[4] == .init(label: "preview", value: "line one\nline two", kind: .lines))
    }

    @Test func theRecordCannotImitateTheSheetsMarkers() {
        let fields = ActionRecord.fields(["changes": [["diff": "-a\n+b"], ["note": "⟨below: changes[0].diff⟩"]]])
        #expect(fields[0].value.contains("⟨U+27E8⟩below"))
        #expect(fields[0].value.components(separatedBy: "⟨below:").count == 2)
    }

    @Test func hiddenCharactersAreShown() {
        #expect(ActionRecord.visible("a\u{202E}b\u{200B}c\u{2028}d\u{07}e\nf\tg") == "a⟨U+202E⟩b⟨U+200B⟩c⟨U+2028⟩d⟨U+0007⟩e\nf\tg")
        #expect(ActionRecord.visible("café 🚀") == "café 🚀")
    }
}

@MainActor @Suite struct LiveApprovalTests {
    func model(_ transport: FakeTransport, approver: TestApprover) throws -> (NeedsYouModel, NeedsYouItem) {
        let client = MainloopAPI.client(origin: origin, middlewares: [], transport: transport)
        let item = try Fixtures.attention().items.map(NeedsYouItem.init)[0]
        return (NeedsYouModel(source: LiveSource(client: client), approver: approver), item)
    }

    @Test func approvingSendsTheSignedApproval() async throws {
        let transport = FakeTransport(json: #"{"status":"ok"}"#, routes: ["getPrivilegedAction": try actionDetailJSON(Fixtures.privilegedRecord().record)])
        let approver = TestApprover()
        let (model, item) = try model(transport, approver: approver)
        await model.reviewAndConfirm(item)

        #expect(await transport.sent.first?.request.path == "/privileged-actions/pa-0001")
        let sent = try #require(await transport.sent.last)
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
        let transport = FakeTransport(
            status: .forbidden, json: #"{"detail":"This device's approval key is not pinned for this action."}"#,
            routes: ["getPrivilegedAction": try actionDetailJSON(Fixtures.privilegedRecord().record)]
        )
        let (model, item) = try model(transport, approver: TestApprover())
        await model.reviewAndConfirm(item)
        #expect(model.lastError == "This device's approval key is not pinned for this action.")
    }

    @Test func aCancelledPromptSendsNoRequest() async throws {
        let transport = FakeTransport(json: #"{"status":"ok"}"#, routes: ["getPrivilegedAction": try actionDetailJSON(Fixtures.privilegedRecord().record)])
        let (model, item) = try model(transport, approver: TestApprover(fails: ApprovalSigningError.cancelled))
        await model.reviewAndConfirm(item)
        #expect(await transport.sent.allSatisfy { $0.request.method == .get })
    }
}
