import MainloopCore
import SwiftUI

/// What Touch ID signs, shown before it asks: the action's canonical record as this iPhone fetched,
/// hashed and parsed it, never the page's card. Hidden characters are shown as ⟨U+XXXX⟩.
struct ApprovalSheet: View {
    @Environment(AppModel.self) private var app
    let review: ApprovalReview

    private var approvals: ApprovalModel { app.approvals }

    var body: some View {
        VStack(spacing: 0) {
            header
            ScrollView {
                VStack(alignment: .leading, spacing: Theme.Space.md) {
                    Panel(title: "action", tint: Theme.warning) {
                        Text(review.record.type).font(Theme.mono(.title3, weight: .semibold)).foregroundStyle(Theme.foreground)
                        FactRow(label: "action id", value: review.record.actionID)
                        FactRow(label: "digest", value: String(review.record.digest.prefix(16)) + "…", tint: Theme.muted)
                    }
                    Panel(title: "arguments") {
                        ForEach(review.record.args, id: \.self) { FieldView(field: $0) }
                    }
                    if !review.record.facts.isEmpty {
                        Panel(title: "what preflight read") {
                            ForEach(review.record.facts, id: \.self) { FieldView(field: $0) }
                        }
                    }
                    if !review.record.context.isEmpty {
                        Panel(title: "request") {
                            ForEach(review.record.context, id: \.self) { FieldView(field: $0) }
                        }
                    }
                    Text("This iPhone read this record from the server and hashed it itself. Approve asks for Touch ID and signs that hash with a key that stays in the Secure Enclave.")
                        .font(Theme.mono(.caption))
                        .foregroundStyle(Theme.muted)
                }
                .padding(Theme.Space.md)
            }
            footer
        }
        .background(Theme.background.ignoresSafeArea())
        .interactiveDismissDisabled(approvals.signing)
        .preferredColorScheme(.dark)
    }

    private var header: some View {
        HStack(spacing: Theme.Space.sm) {
            Text(">").foregroundStyle(Color(hex: Palette.markPrompt))
            Text("approve privileged action").foregroundStyle(Theme.foreground)
            Spacer()
        }
        .font(Theme.mono(.headline, weight: .semibold))
        .padding(Theme.Space.md)
        .overlay(alignment: .bottom) { Rectangle().fill(Theme.borderSubtle).frame(height: 1) }
    }

    private var footer: some View {
        HStack(spacing: Theme.Space.sm) {
            Button("Cancel") { approvals.cancel() }
                .buttonStyle(TerminalButtonStyle())
                .disabled(approvals.signing)
            Button {
                Task { await approvals.confirm() }
            } label: {
                HStack(spacing: Theme.Space.sm) {
                    if approvals.signing { ProgressView().tint(Theme.background) } else { Image(systemName: "touchid") }
                    Text("Approve")
                }
            }
            .buttonStyle(TerminalButtonStyle(prominent: true))
            .disabled(approvals.signing)
        }
        .padding(Theme.Space.md)
        .overlay(alignment: .top) { Rectangle().fill(Theme.borderSubtle).frame(height: 1) }
    }
}

private struct FieldView: View {
    let field: ActionRecord.Field

    var body: some View {
        VStack(alignment: .leading, spacing: 2) {
            Text(field.label).font(Theme.mono(.caption)).foregroundStyle(Theme.muted)
            switch field.kind {
            case .text, .command:
                Text(field.value).font(Theme.mono(.callout)).foregroundStyle(Theme.foreground).textSelection(.enabled)
            case .lines:
                ScrollView(.horizontal) {
                    Text(field.value).font(Theme.mono(.caption)).foregroundStyle(Theme.foreground).fixedSize().textSelection(.enabled)
                }
            case .diff:
                ScrollView(.horizontal) {
                    VStack(alignment: .leading, spacing: 0) {
                        ForEach(Array(field.value.split(separator: "\n", omittingEmptySubsequences: false).enumerated()), id: \.offset) { _, line in
                            DiffLine(line: String(line))
                        }
                    }
                    .fixedSize()
                    .textSelection(.enabled)
                }
                .background(Theme.background)
                .overlay(Rectangle().strokeBorder(Theme.borderSubtle, lineWidth: 1))
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }
}

/// One line of a unified diff, in the theme's colours: added green, removed red, headers muted.
private struct DiffLine: View {
    let line: String

    var body: some View {
        Text(line.isEmpty ? " " : line)
            .font(Theme.mono(.caption))
            .foregroundStyle(color)
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(.horizontal, Theme.Space.xs)
            .background(background)
    }

    private var isHeader: Bool { line.hasPrefix("+++") || line.hasPrefix("---") || line.hasPrefix("@@") }

    private var color: Color {
        if isHeader { return Theme.muted }
        if line.hasPrefix("+") { return Theme.green }
        if line.hasPrefix("-") { return Theme.error }
        return Theme.foreground
    }

    private var background: Color {
        if isHeader { return .clear }
        if line.hasPrefix("+") { return Theme.green.opacity(0.12) }
        if line.hasPrefix("-") { return Theme.error.opacity(0.12) }
        return .clear
    }
}
