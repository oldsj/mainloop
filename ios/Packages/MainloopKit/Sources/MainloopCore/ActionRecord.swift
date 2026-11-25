import CryptoKit
import Foundation

/// A privileged action as the phone approves it (docs/specs/privileged-actions.md, "Signed
/// approvals"): the canonical record from `GET /privileged-actions/{id}`, hashed here, never the
/// server's word for its digest or the card's text.
///
/// The digest the phone signs is the SHA-256 of exactly these bytes, and everything the approval
/// sheet shows is parsed from them. The executor runs an action only when the canonical record it
/// computes from the stored row has that digest, so what runs is what the sheet showed.
public struct ActionRecord: Hashable, Sendable {
    /// One labelled value on the sheet.
    public struct Field: Hashable, Sendable {
        public let label: String
        public let value: String
    }

    public let actionID: String
    public let type: String
    /// SHA-256 of the record's bytes, 64 lowercase hex characters, computed on the phone.
    public let digest: String
    /// The arguments, one field per key, in key order.
    public let args: [Field]
    /// What preflight read, one field per key, in key order: the diff, the current state.
    public let facts: [Field]
    /// Who asked, when the request expires, and the versions.
    public let context: [Field]

    /// What the Touch ID prompt says.
    public var reason: String { "Approve \(type) (\(actionID))" }

    /// Hashes `record` and checks it is the card's: the same action id and digest, and the same
    /// type when the card names one. Throws when the record doesn't parse as the canonical form.
    public static func verify(_ record: String, for card: PrivilegedCard) throws -> ActionRecord {
        let bytes = Data(record.utf8)
        let digest = SHA256.hash(data: bytes).map { String(format: "%02x", $0) }.joined()
        guard let expected = card.approvable else { throw ActionRecordError.notApprovable }
        guard digest == expected.digest else { throw ActionRecordError.digestMismatch }
        guard let object = try? JSONSerialization.jsonObject(with: bytes, options: [.fragmentsAllowed]) as? [String: Any],
              let actionID = object["action_id"] as? String, let type = object["type"] as? String,
              let args = object["args"] as? [String: Any]
        else { throw ActionRecordError.malformed }
        guard actionID == expected.actionID else { throw ActionRecordError.otherAction }
        if let cardType = card.type, cardType != type { throw ActionRecordError.otherAction }
        let facts = object["frozen_facts"] as? [String: Any] ?? [:]
        var context: [Field] = []
        if let requester = object["requester"] as? [String: Any] {
            let who = [requester["role"], requester["session_id"], requester["task_id"]]
                .compactMap { $0.flatMap { $0 as? String } }.joined(separator: " · ")
            context.append(Field(label: "Requested by", value: visible(who)))
        }
        if let expires = object["expires_at"] as? String { context.append(Field(label: "Expires", value: visible(expires))) }
        context.append(Field(label: "Action id", value: visible(actionID)))
        context.append(Field(label: "Digest", value: digest))
        return ActionRecord(
            actionID: actionID, type: type, digest: digest,
            args: fields(args), facts: fields(facts), context: context
        )
    }

    static func fields(_ object: [String: Any]) -> [Field] {
        object.keys.sorted().map { key in Field(label: visible(key), value: display(object[key] as Any)) }
    }

    /// A value as the sheet shows it: a string as is, a list of strings (an argv) joined by
    /// spaces, anything else as indented JSON. Always through `visible`.
    static func display(_ value: Any) -> String {
        switch value {
        case let string as String: return visible(string)
        case let list as [Any] where !list.isEmpty && list.allSatisfy({ $0 is String }):
            return visible((list as? [String] ?? []).joined(separator: " "))
        case is NSNull: return "null"
        default:
            let options: JSONSerialization.WritingOptions = [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes, .fragmentsAllowed]
            guard let data = try? JSONSerialization.data(withJSONObject: value, options: options) else { return "?" }
            return visible(String(decoding: data, as: UTF8.self))
        }
    }

    /// Text with every character that could hide or reorder what is shown made visible as
    /// ⟨U+XXXX⟩: control characters but newline and tab, format characters such as bidi overrides
    /// and zero-width joiners, and line and paragraph separators.
    public static func visible(_ text: String) -> String {
        var out = ""
        for scalar in text.unicodeScalars {
            let hidden: Bool = switch scalar.properties.generalCategory {
            case .control: scalar != "\n" && scalar != "\t"
            case .format, .lineSeparator, .paragraphSeparator, .unassigned, .privateUse, .surrogate: true
            default: false
            }
            if hidden {
                out += String(format: "⟨U+%04X⟩", scalar.value)
            } else {
                out.unicodeScalars.append(scalar)
            }
        }
        return out
    }
}

public enum ActionRecordError: Error, Equatable, Sendable {
    /// The card's action id or digest is missing or malformed.
    case notApprovable
    /// The record's SHA-256 isn't the card's digest.
    case digestMismatch
    /// The record names another action or type than the card.
    case otherAction
    /// The record isn't the canonical form, or the action has no frozen facts yet.
    case malformed
}
