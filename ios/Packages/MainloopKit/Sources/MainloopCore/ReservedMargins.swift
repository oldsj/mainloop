import Foundation

/// Insets on each edge. SwiftUI-free, so the reserved-region math is testable on macOS.
public struct EdgeMargins: Equatable, Sendable {
    public var top: Double
    public var leading: Double
    public var bottom: Double
    public var trailing: Double

    public init(top: Double = 0, leading: Double = 0, bottom: Double = 0, trailing: Double = 0) {
        self.top = top
        self.leading = leading
        self.bottom = bottom
        self.trailing = trailing
    }

    public static let zero = EdgeMargins()

    /// The margins that keep content clear of every given region: the largest per edge.
    /// On iPhone Duo these are the active occlusion regions (the outer and inner cameras); the
    /// fold (a division region) is left to the split arrangement, which places its panes around it.
    public static func union(_ margins: [EdgeMargins]) -> EdgeMargins {
        margins.reduce(.zero) { acc, m in
            EdgeMargins(
                top: max(acc.top, m.top), leading: max(acc.leading, m.leading),
                bottom: max(acc.bottom, m.bottom), trailing: max(acc.trailing, m.trailing)
            )
        }
    }

    /// What to add on top of the safe area, which may already cover part of a region.
    public func subtracting(_ other: EdgeMargins) -> EdgeMargins {
        EdgeMargins(
            top: max(0, top - other.top), leading: max(0, leading - other.leading),
            bottom: max(0, bottom - other.bottom), trailing: max(0, trailing - other.trailing)
        )
    }
}
