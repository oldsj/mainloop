# Status badge

Source: `docs/specs/sessions.md`, `docs/specs/tasks.md`.

## Purpose

A status word in its token colour: session status in the list and session page, task status in the task tree, and "needs you" attention. The words say what is happening; the colour only reinforces them.

## Tokens

- Tones map to the terminal tokens: active `term-cyan`, attention `term-yellow`, ok `term-green`, bad `term-red`, muted `term-fg-muted` with `term-border`.
- The border is the tone colour at half strength. Text is uppercase by CSS; the label in the DOM keeps its words.
- Radius `rounded-none`. Spacing `px-sm py-xs`. Type `text-xs`, medium weight.

## States

Each session status has one tone, set in one place (`SESSION_STATUS_TONE`):

- **active, pending:** running or about to run. Tone active.
- **needs input or needs you:** the agent waits for the owner. Tone attention. "NEEDS YOU" is shown for an idle session that needs the owner.
- **done:** completed. Tone ok.
- **failed:** tone bad.
- **idle, superseded, cancelled:** finished or parked, nothing to act on. Tone muted.

An unknown status shows its own words in muted tone rather than failing the row.

## Keyboard and screen reader

- The badge is plain text in the row. It is not focusable and needs no role.
- Read in place: the words are the meaning, so a screen reader hears "NEEDS INPUT" with the row, not a colour.
- Do not announce a badge on its own. A change of status is announced by the surface that changed it, once.

## Touch

- The badge is not a control, so it has no minimum size. If a badge becomes a button, it gets the 44 px target.

## Behaviour that must hold

- The label is never empty and never only a colour. A status a user cannot read must not look fine.
- A status dropped from the contract (C1c: `implementing`, `under_review`) is not given a tone of its own. It shows muted.

## Do

- Use the same words everywhere: NEEDS INPUT, NEEDS YOU, DONE, FAILED.
- Keep the tone decision in `SESSION_STATUS_TONE`, not in each component.

## Don't

- Don't use a palette colour (red-500, green-600). Tokens only.
- Don't use colour alone to show attention. The word is the signal.
- Don't introduce a fourth tone for "sending". The server reports the status it has.
