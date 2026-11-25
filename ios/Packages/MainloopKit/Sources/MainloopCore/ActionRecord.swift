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
        public enum Kind: Hashable, Sendable {
            /// A short value, or indented JSON.
            case text
            /// A list of words, such as an argv, quoted as one would type it.
            case command
            /// A unified diff, shown line by line.
            case diff
            /// Text of several lines, shown as is.
            case lines
        }

        public let label: String
        public let value: String
        public let kind: Kind

        public init(label: String, value: String, kind: Kind = .text) {
            self.label = label
            self.value = value
            self.kind = kind
        }
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

    /// One field per key, in key order. A string of several lines nested in a value, such as a
    /// diff in `changes[0].diff`, gets a field of its own right after it, and the value's JSON
    /// says where it went.
    static func fields(_ object: [String: Any]) -> [Field] {
        var out: [Field] = []
        for key in object.keys.sorted() {
            let value = object[key] as Any
            if let text = value as? String, text.contains("\n") {
                out.append(Field(label: visible(key), value: visible(text), kind: kind(of: text, key: key)))
                continue
            }
            var moved: [Field] = []
            let token = UUID().uuidString
            let kept = extract(value, path: visible(key), key: key, token: token, into: &moved)
            var (text, kind) = display(kept)
            for (i, m) in moved.enumerated() {
                text = text.replacingOccurrences(of: "\(token)#\(i)#", with: "⟨below: \(m.label)⟩")
            }
            out.append(Field(label: visible(key), value: text, kind: kind))
            out += moved
        }
        return out
    }

    /// `value` with every string of several lines replaced by a placeholder (`token`#n#), which the
    /// record's own text can't contain, and those strings appended to `moved`.
    static func extract(_ value: Any, path: String, key: String, token: String, into moved: inout [Field]) -> Any {
        switch value {
        case let text as String where text.contains("\n"):
            moved.append(Field(label: path, value: visible(text), kind: kind(of: text, key: key)))
            return "\(token)#\(moved.count - 1)#"
        case let object as [String: Any]:
            var out: [String: Any] = [:]
            for (k, v) in object {
                out[k] = extract(v, path: path + "." + visible(k), key: k, token: token, into: &moved)
            }
            return out
        case let list as [Any]:
            return list.enumerated().map { i, v in extract(v, path: path + "[\(i)]", key: key, token: token, into: &moved) }
        default:
            return value
        }
    }

    static func kind(of text: String, key: String) -> Field.Kind {
        key == "diff" || text.hasPrefix("--- ") || text.hasPrefix("diff ") || text.contains("\n@@ ") ? .diff : .lines
    }

    /// A value as the sheet shows it: a string as is, a list of strings (an argv) quoted as one
    /// would type it, anything else as indented JSON. Always through `visible`.
    static func display(_ value: Any) -> (value: String, kind: Field.Kind) {
        switch value {
        case let string as String: return (visible(string), .text)
        case let list as [Any] where !list.isEmpty && list.allSatisfy({ $0 is String }):
            return (visible(shellQuote(list.compactMap { $0 as? String })), .command)
        case is NSNull: return ("null", .text)
        default:
            let options: JSONSerialization.WritingOptions = [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes, .fragmentsAllowed]
            guard let data = try? JSONSerialization.data(withJSONObject: value, options: options) else { return ("?", .text) }
            return (visible(String(decoding: data, as: UTF8.self)), .text)
        }
    }

    static let shellSafe = Set("-_./:=,@abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789")

    /// Words as one would type them, the server's rule (privileged.shellQuote, the web card's): a
    /// word of safe characters as is, any other in single quotes, a quote inside as '\''.
    public static func shellQuote(_ words: [String]) -> String {
        words.map { word in
            if !word.isEmpty, word.allSatisfy({ shellSafe.contains($0) }) { return word }
            return "'" + word.replacingOccurrences(of: "'", with: #"'\''"#) + "'"
        }.joined(separator: " ")
    }

    /// Text with every character that could hide or reorder what is shown made visible as
    /// ⟨U+XXXX⟩: control characters but newline and tab, format characters such as bidi overrides
    /// and zero-width joiners, line and paragraph separators, and ⟨ itself.
    public static func visible(_ text: String) -> String {
        var out = ""
        for scalar in text.unicodeScalars {
            let hidden: Bool = switch scalar.properties.generalCategory {
            case .control: scalar != "\n" && scalar != "\t"
            case .format, .lineSeparator, .paragraphSeparator, .unassigned, .privateUse, .surrogate: true
            // The sheet's own markers start with ⟨, so the record's text can't imitate one.
            default: scalar == "⟨"
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
