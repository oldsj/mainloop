import CryptoKit
import Foundation
import HTTPTypes
import MainloopAPI
@testable import MainloopCore
import Testing

/// An approver with a software key, or one whose prompt fails as given.
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

let request = ApprovalRequest(queueItemID: "q-0001", actionID: "pa-0001")!

extension ApprovalModel {
    /// Asks as the page does, lets `owner` act on the sheet once it opens, and returns the page's answer.
    func ask(_ request: ApprovalRequest, owner: @escaping @MainActor (ApprovalModel) async -> Void) async -> ApprovalOutcome {
        let asked = Task { await self.request(request) }
        for _ in 0..<2000 where reviewing == nil {
            try? await Task.sleep(for: .milliseconds(1))
        }
        if reviewing != nil { await owner(self) }
        return await asked.value
    }
}

@MainActor @Suite struct ApprovalModelTests {
    func model(_ transport: FakeTransport, approver: TestApprover? = TestApprover()) -> ApprovalModel {
        let client = MainloopAPI.client(origin: origin, middlewares: [], transport: transport)
        return ApprovalModel(service: LiveApprovalService(client: client), approver: approver)
    }

    func served(_ record: String? = nil, status: HTTPResponse.Status = .ok, json: String = #"{"status":"ok","message":"Response sent"}"#) throws -> FakeTransport {
        FakeTransport(status: status, json: json, routes: ["getPrivilegedAction": try actionDetailJSON(record ?? Fixtures.privilegedRecord().record)])
    }

    @Test func theSheetShowsTheRecordThePhoneHashedThenSendsTheSignedApproval() async throws {
        let transport = try served()
        let approver = TestApprover()
        let model = model(transport, approver: approver)
        var shown: ApprovalReview?
        let outcome = await model.ask(request) { model in
            shown = model.reviewing
            await model.confirm()
        }
        #expect(outcome == .approved)
        #expect(model.reviewing == nil)
        let review = try #require(shown)
        #expect(review.record.digest == fixtureDigest)
        #expect(review.record.type == "k8s.rollout_restart")
        #expect(review.record.args.first { $0.label == "namespace" }?.value == "shop")
        // A bidi override is shown, never applied.
        let note = try #require(review.record.facts.first { $0.label == "note" }?.value)
        #expect(note.contains("⟨U+202E⟩evil"))
        #expect(review.record.reason == "Approve k8s.rollout_restart (pa-0001)")

        #expect(await transport.sent.first?.request.path == "/privileged-actions/pa-0001")
        let sent = try #require(await transport.sent.last)
        #expect(sent.request.path == "/queue/q-0001/respond")
        let body = try JSONSerialization.jsonObject(with: try #require(sent.body)) as! [String: Any]
        #expect(body["response"] as? String == "Approve")
        let metadata = try #require(body["metadata"] as? [String: String])
        // The digest the phone computed, never one the page sent.
        #expect(metadata["reviewed_context"] == fixtureDigest)
        let claims = try DecodedToken(try #require(metadata["approval"]), key: approver.signer.key.publicKey).claims
        #expect(claims["act"] as? String == "pa-0001")
        #expect(claims["dig"] as? String == fixtureDigest)
    }

    @Test func aRecordForAnotherActionIsRefusedAndNothingIsSigned() async throws {
        // The page names pa-0002; the server serves pa-0001's record.
        let transport = try served()
        let model = model(transport)
        let outcome = await model.request(ApprovalRequest(queueItemID: "q-0001", actionID: "pa-0002")!)
        #expect(outcome == .failed(ApprovalModel.describe(ActionRecordError.otherAction)))
        #expect(model.reviewing == nil)
        #expect(await transport.sent.allSatisfy { $0.request.method == .get })
    }

    @Test func cancellingTheSheetSendsNothing() async throws {
        let transport = try served()
        let model = model(transport)
        let outcome = await model.ask(request) { $0.cancel() }
        #expect(outcome == .cancelled)
        #expect(await transport.sent.allSatisfy { $0.request.method == .get })
    }

    @Test func aCancelledTouchIDPromptKeepsTheSheet() async throws {
        let transport = try served()
        let model = model(transport, approver: TestApprover(fails: ApprovalSigningError.cancelled))
        let outcome = await model.ask(request) { model in
            await model.confirm()
            #expect(model.reviewing != nil)
            model.cancel()
        }
        #expect(outcome == .cancelled)
        #expect(await transport.sent.allSatisfy { $0.request.method == .get })
    }

    @Test func anUnrecognisedFingerSendsNothingAndSaysWhy() async throws {
        let transport = try served()
        let model = model(transport, approver: TestApprover(fails: ApprovalSigningError.notRecognised))
        let outcome = await model.ask(request) { await $0.confirm() }
        #expect(outcome == .failed("Touch ID didn't recognise you. Nothing was sent."))
        #expect(await transport.sent.allSatisfy { $0.request.method == .get })
    }

    @Test func aRefusalShowsTheServersReason() async throws {
        let transport = try served(status: .forbidden, json: #"{"detail":"This device's approval key is not pinned for this action."}"#)
        let model = model(transport)
        let outcome = await model.ask(request) { await $0.confirm() }
        #expect(outcome == .failed("This device's approval key is not pinned for this action."))
    }

    @Test func anUnpairedPhoneSaysSo() async throws {
        let unpaired = ApprovalModel(service: nil, approver: nil)
        #expect(await unpaired.request(request) == .failed(ApprovalModel.notPaired))
        let keyless = model(try served(), approver: TestApprover(canApprove: false))
        #expect(await keyless.request(request) == .failed(ApprovalModel.notPaired))
    }

    @Test func oneApprovalAtATime() async throws {
        let model = model(try served())
        let outcome = await model.ask(request) { model in
            #expect(await model.request(request) == .failed(ApprovalModel.busy))
            model.cancel()
        }
        #expect(outcome == .cancelled)
    }

    @Test func theRepliesThePageReads() {
        #expect(ApprovalOutcome.approved.reply == ["status": "approved"])
        #expect(ApprovalOutcome.cancelled.reply == ["status": "cancelled"])
        #expect(ApprovalOutcome.failed("No.").reply == ["status": "failed", "message": "No."])
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
        let record = try ActionRecord.read(vector.record, actionID: "pa-0001")
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

