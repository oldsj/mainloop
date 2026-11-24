// PrepareContract <in.yaml> <out.yaml>
//
// swift-openapi-generator 1.14 reads two things in api/openapi.yaml badly, so this rewrites a
// temporary copy before generating:
//
// 1. A nullable value written the OpenAPI 3.1 way, `anyOf: [X, {type: 'null'}]` (the Go server's
//    contract style), is skipped by the generator: the property disappears from the Swift type.
//    The null branch is removed (a single remaining branch is inlined), and the property is
//    dropped from its schema's `required`. Swift then gets `X?`, whose decoder reads both an
//    absent key and JSON null as nil, which is what the contract allows.
// 2. Its parser reads integer bounds as Int64, so a `maximum` or `minimum` beyond Int64 (a
//    uint64 bound) fails the whole document. Such a bound is removed; it only validated.
//
// Nothing else changes. The app never validates bounds the server already enforces.
import Foundation
import Yams

let args = CommandLine.arguments
guard args.count == 3 else {
    FileHandle.standardError.write(Data("usage: PrepareContract <in.yaml> <out.yaml>\n".utf8))
    exit(2)
}
let text = try String(contentsOfFile: args[1], encoding: .utf8)
guard let root = try Yams.compose(yaml: text) else { fatalError("empty document") }

func isNullSchema(_ node: Node) -> Bool {
    guard let map = node.mapping, map.count == 1 else { return false }
    return map["type"]?.string == "null"
}

/// Whether a schema is `anyOf`/`oneOf` with a null branch.
func hasNullBranch(_ node: Node) -> Bool {
    guard let map = node.mapping else { return false }
    for key in ["anyOf", "oneOf"] {
        if let seq = map[key]?.sequence, seq.contains(where: isNullSchema) { return true }
    }
    return false
}

struct Rewriter {
var nullableRewrites = 0
var boundsDropped = 0

mutating func rewrite(_ node: Node) -> Node {
    switch node {
    case .sequence(var seq):
        seq = Node.Sequence(seq.map { rewrite($0) }, seq.tag, seq.style)
        return .sequence(seq)
    case .mapping(var map):
        // Properties that may be null are not required in Swift: X? covers absent and null.
        if let props = map["properties"]?.mapping, let required = map["required"]?.sequence {
            let nullable = Set(props.compactMap { key, value in hasNullBranch(value) ? key.string : nil })
            if !nullable.isEmpty {
                let kept = required.filter { !nullable.contains($0.string ?? "") }
                if kept.isEmpty {
                    map["required"] = nil
                } else {
                    map["required"] = .sequence(Node.Sequence(Array(kept), required.tag, required.style))
                }
            }
        }
        for key in ["maximum", "minimum"] {
            if let value = map[key], case .scalar(let scalar) = value, Int64(scalar.string) == nil,
               Double(scalar.string) != nil, !scalar.string.contains(".") {
                map[key] = nil
                boundsDropped += 1
            }
        }
        for key in ["anyOf", "oneOf"] {
            guard let seq = map[key]?.sequence, seq.contains(where: isNullSchema) else { continue }
            let rest = seq.filter { !isNullSchema($0) }
            nullableRewrites += 1
            if rest.count == 1, let only = rest.first?.mapping {
                map[key] = nil
                for (k, v) in only where map[k] == nil { map[k] = v }
            } else {
                map[key] = .sequence(Node.Sequence(Array(rest), seq.tag, seq.style))
            }
        }
        var out = Node.Mapping([], map.tag, map.style)
        for (k, v) in map { out[k] = rewrite(v) }
        return .mapping(out)
    default:
        return node
    }
}
}

var rewriter = Rewriter()
let result = rewriter.rewrite(root)
try Yams.serialize(node: result).write(toFile: args[2], atomically: true, encoding: .utf8)
FileHandle.standardError.write(Data(
    "PrepareContract: \(rewriter.nullableRewrites) nullable anyOf/oneOf rewritten, \(rewriter.boundsDropped) out-of-range bounds dropped\n".utf8))
