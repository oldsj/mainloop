# Session controls

Source: `docs/specs/chat.md` (main provider), `docs/specs/sessions.md` (live sessions).

## Purpose

One chip below the composer that says how the session runs, and the one picker it opens. It replaces the provider select in the header and the model button in the live strip, so provider, model and effort are chosen in one place, next to the permission mode.

## Tokens

- Chip: plain text in `text-muted-foreground`, `text-foreground` on hover and while open, followed by `▾`. It takes the control height (`min-h-control`).
- Picker: `bg-popover text-popover-foreground` with `border-border` and `shadow-md`, padding `p-md`. Section headings are small muted capitals.
- Options: `border-border-subtle`, `hover:bg-accent`; the chosen one fills with `bg-term-selection`.
- Confirmation: a block edged with `border-status-attention/50`. **Switch to <provider>** is the primary button; **Cancel** is a ghost button.

## Layout

- **Phone:** the picker is a sheet across the bottom of the screen over a dimmed page, with a **Close** button.
- **Wide screen (`md` and up):** the picker is 420 px wide and opens just above the chip. The page is not dimmed.

## States

- **Label:** the provider (main thread only), the model without its `claude-` prefix, and the effort, joined by `·`, for example `Claude · opus-5-5 · high`. A part that is not known is left out. An unknown provider reads "provider unknown".
- **Sections:** Provider (main thread only), then Model and Effort under the live view. In the chat view the picker holds Provider and a line saying to type `/model` or `/effort`.
- **Choosing a provider:** asks first. The confirmation says that switching starts a fresh native session and that the conversation, tasks and earlier native history are kept. Nothing is sent until **Switch to <provider>**; the picker then closes. Choosing the ready provider again asks nothing.
- **Held:** while the provider cannot change (offline, a send or stop in flight, the thread still loading, a switch pending, or a turn or delivery open) both provider options are disabled and a line says why. An open confirmation is dropped. Model and effort stay available.
- **Model and effort:** choosing one sets it for the session's next turns and closes the picker. `/model` and `/effort` without an argument open the same picker.
- **Trouble:** a failed, switching or unconfirmed provider is not shown here. It shows under the main thread header's row, with Retry or Restart.

## Keyboard and screen reader

- The chip is a button whose name is its label; it sets `aria-haspopup="dialog"` and `aria-expanded`.
- The picker is a `role="dialog"` named "Session controls". Opening it moves focus into it; Escape or a press outside closes it and returns focus to where it was.
- Provider, model and effort are radio groups ("Main provider", "Models", "Effort") whose options set `aria-checked`.
- The confirmation is a group named "Switch to <provider>".

## Touch

- The chip, the provider and effort options, Close and both confirmation buttons take the control height: at least 44 px on touch. A model option is taller, since it carries a description.

## Do

- Keep it the only place the provider, model and effort are chosen.
- Say what a switch keeps and what it starts, in the confirmation itself.

## Don't

- Don't switch the provider on a single press.
- Don't show status here. The session's status is the one word in the session row.
- Don't hide a provider problem inside the picker.
