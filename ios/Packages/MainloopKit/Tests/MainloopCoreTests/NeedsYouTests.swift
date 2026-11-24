import Foundation
import MainloopAPI
@testable import MainloopCore
import Testing

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
        ["privileged_action_digest": String(repeating: "a", count: 64)],
    ] as [[String: String]])
    func rejectsAMalformedPrivilegedContext(_ context: [String: String]) {
        #expect(PrivilegedCard(context: context) == nil)
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

    @Test func sessionsListLiveAndNeedsOwnerFirstWithoutArchived() async throws {
        let model = SessionsModel(source: try FixtureSource())
        await model.load()
        #expect(model.rows.map(\.id) == ["sess-sup-0002", "sess-main-0001", "sess-task-0003"])
        #expect(model.rows[0].status == .waitingOnUser)
        #expect(model.rows[0].status.label == "Waiting on you")
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
