/// The web app's default theme, `eighties` (web/design/tokens.json, `color.theme.eighties.term`), for
/// the few native screens: first run, the approval sheet and This iPhone. A test checks every value
/// against the tokens, so the two can't drift. Square corners and one monospaced family, as on the web.
public enum Palette {
    public static let background = "#2d2d2d"
    public static let card = "#393939"
    public static let foreground = "#d3d0c8"
    public static let muted = "#a3a399"
    public static let border = "#767676"
    public static let borderSubtle = "#4e4e4e"
    public static let accent = "#79a6d2"
    public static let green = "#99cc99"
    public static let warning = "#f99157"
    public static let error = "#f38082"
    public static let selection = "#515151"

    /// Token name (under `color.theme.eighties.term`) → value, for the test.
    public static let tokens: [String: String] = [
        "bg": background, "bg-secondary": card, "fg": foreground, "fg-muted": muted, "border": border,
        "border-subtle": borderSubtle, "accent": accent, "green": green, "warning": warning, "error": error,
        "selection": selection,
    ]

    /// The mark's colours (web/public/favicon.svg): `>` and `_` on the background.
    public static let markPrompt = "#6699cc"
    public static let markCursor = "#99cc99"
}
