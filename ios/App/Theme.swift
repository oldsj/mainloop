import MainloopCore
import SwiftUI
import UIKit

/// The web app's look for the native screens (first run, the approval sheet, This iPhone): the
/// default theme's colours (`Palette`, checked against web/design/tokens.json), square corners, and
/// one monospaced family. The web's font stack starts with `ui-monospace`, which WebKit resolves to
/// SF Mono, so the system's monospaced design here is the font the web view shows.
enum Theme {
    static let background = Color(hex: Palette.background)
    static let card = Color(hex: Palette.card)
    static let foreground = Color(hex: Palette.foreground)
    static let muted = Color(hex: Palette.muted)
    static let border = Color(hex: Palette.border)
    static let borderSubtle = Color(hex: Palette.borderSubtle)
    static let accent = Color(hex: Palette.accent)
    static let green = Color(hex: Palette.green)
    static let warning = Color(hex: Palette.warning)
    static let error = Color(hex: Palette.error)
    static let selection = Color(hex: Palette.selection)

    static let uiBackground = UIColor(background)

    /// The web's spacing scale (tokens.json `spacing`, at 16 px per rem).
    enum Space {
        static let xs: CGFloat = 4
        static let sm: CGFloat = 8
        static let md: CGFloat = 16
        static let lg: CGFloat = 24
        static let xl: CGFloat = 32
    }

    /// `size.control.touch`: the minimum height of anything tapped.
    static let control: CGFloat = 44

    static func mono(_ style: Font.TextStyle = .body, weight: Font.Weight = .regular) -> Font {
        .system(style, design: .monospaced, weight: weight)
    }
}

extension Color {
    /// `#rrggbb`.
    init(hex: String) {
        let value = UInt64(hex.dropFirst(), radix: 16) ?? 0
        self.init(
            .sRGB,
            red: Double((value >> 16) & 0xFF) / 255,
            green: Double((value >> 8) & 0xFF) / 255,
            blue: Double(value & 0xFF) / 255
        )
    }
}

/// The mark, `>_`, drawn as web/public/favicon.svg draws it on its 32-unit grid.
struct Mark: View {
    var size: CGFloat = 64

    var body: some View {
        Canvas { context, canvas in
            let unit = canvas.width / 32
            context.fill(Path(CGRect(origin: .zero, size: canvas)), with: .color(Theme.background))
            var prompt = Path()
            prompt.move(to: CGPoint(x: 8 * unit, y: 10 * unit))
            prompt.addLine(to: CGPoint(x: 15 * unit, y: 16 * unit))
            prompt.addLine(to: CGPoint(x: 8 * unit, y: 22 * unit))
            context.stroke(prompt, with: .color(Color(hex: Palette.markPrompt)), style: StrokeStyle(lineWidth: 3 * unit, miterLimit: 4))
            var cursor = Path()
            cursor.move(to: CGPoint(x: 17 * unit, y: 23 * unit))
            cursor.addLine(to: CGPoint(x: 25 * unit, y: 23 * unit))
            context.stroke(cursor, with: .color(Color(hex: Palette.markCursor)), style: StrokeStyle(lineWidth: 3 * unit))
        }
        .frame(width: size, height: size)
        .overlay(Rectangle().strokeBorder(Theme.borderSubtle, lineWidth: 1))
        .accessibilityLabel("Mainloop")
    }
}

/// A square, bordered control in the web's style. `prominent` is the screen's one primary action.
struct TerminalButtonStyle: ButtonStyle {
    var prominent = false
    var destructive = false
    @Environment(\.isEnabled) private var isEnabled

    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .font(Theme.mono(.body, weight: .medium))
            .frame(maxWidth: .infinity, minHeight: Theme.control)
            .padding(.horizontal, Theme.Space.md)
            .foregroundStyle(prominent ? Theme.background : destructive ? Theme.error : Theme.foreground)
            .background(prominent ? Theme.accent : configuration.isPressed ? Theme.selection : Theme.card)
            .overlay(Rectangle().strokeBorder(prominent ? Theme.accent : destructive ? Theme.error : Theme.border, lineWidth: 1))
            .opacity(isEnabled ? (configuration.isPressed ? 0.85 : 1) : 0.5)
            .contentShape(Rectangle())
    }
}

/// A bordered block on the card colour, with an optional `# heading` line.
struct Panel<Content: View>: View {
    var title: String?
    var tint: Color = Theme.borderSubtle
    @ViewBuilder var content: () -> Content

    var body: some View {
        VStack(alignment: .leading, spacing: Theme.Space.sm) {
            if let title {
                Text("# " + title).font(Theme.mono(.footnote, weight: .semibold)).foregroundStyle(Theme.muted)
            }
            content()
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(Theme.Space.md)
        .background(Theme.card)
        .overlay(Rectangle().strokeBorder(tint, lineWidth: 1))
    }
}

/// A label and its value, one under the other.
struct FactRow: View {
    let label: String
    let value: String
    var tint: Color = Theme.foreground

    var body: some View {
        VStack(alignment: .leading, spacing: 2) {
            Text(label).font(Theme.mono(.caption)).foregroundStyle(Theme.muted)
            Text(value).font(Theme.mono(.callout)).foregroundStyle(tint).textSelection(.enabled)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }
}

/// A sentence that explains a problem and what to do, never a raw error.
struct Notice: View {
    let text: String
    var tint: Color = Theme.warning

    var body: some View {
        Text(text)
            .font(Theme.mono(.callout))
            .foregroundStyle(tint)
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(Theme.Space.md)
            .overlay(Rectangle().strokeBorder(tint.opacity(0.6), lineWidth: 1))
    }
}
