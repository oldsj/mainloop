# Session summary

Source: `docs/specs/sessions.md`, `docs/specs/chat.md` (Sending messages).

## Purpose

The row in the session list and the header and identity strip at the top of a session view. It answers: what is this session, is it working, and does it need me?

## Tokens

- Row `bg-background` with `border-border`. The open row (`aria-current`) and a hovered row fill with `bg-accent`, and all their text is `text-accent-foreground`, including metadata and status words, so every word stays at 4.5:1 on the fill.
- The open row also shows a `▌` marker before its title and a bold title. The fill is too close to the plain row to carry the state alone (about 1.5 to 1.9:1 against `bg`), so the marker and weight do.
- On the plain row, status words use the same tokens as the delivery receipt and metadata uses `text-muted-foreground`. The error line keeps its `Error:` label on the fill.
- Radius `rounded-none`.

## What the strip shows

- Agent kind, model, the kagent Agent name, and the Session state.
- Turn count and the delivery states.
- The reason for the last failed delivery, if there is one.
- The status word: ready, working, last message failed, or last message unconfirmed.

## States

- **Empty:** "No sessions" with a short line on how one starts.
- **Loading:** rows as skeletons with the same height as real rows.
- **Stale:** show the time of the last update next to the status.
- **Error:** keep the last known list and show "Retry loading sessions".
- **Offline:** show the last known list with its time. Do not show a status word that the server did not report.

## Keyboard and screen reader

- Each row is one link or button to the session. Its name includes the session title and status word.
- The identity strip is a definition list (label and value pairs), so a screen reader reads each value with its label.
- The history toggle is a switch with a clear label. Turning it on or off does not archive or delete anything; say so in the label.

## Touch

- Each row is at least 44 px tall. The history toggle is at least 44 px.

## Do

- Show the same status word in the list and the header.
- Keep long session names on one line with an ellipsis; show the full name in the header.

## Don't

- Don't mark a session "done" from a finished native turn alone.
- Don't hide the failure reason in the strip; it is the first thing someone needs.
- Don't show raw provider output in the summary.
