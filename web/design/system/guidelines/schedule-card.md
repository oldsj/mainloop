# Schedule card

Source: `docs/specs/scheduled-runs.md` (UI section), `web/src/features/schedules/ScheduleCard.tsx`.

## Purpose

One scheduled run at a glance: its name, when it runs in plain words, whether it is active or paused, which agent it starts and when it runs next. The list uses it as a link to the schedule. The detail page uses it as the summary above the edit form.

## Tokens

- Name `text-foreground`, medium weight, `text-sm`.
- Plain-words line `text-foreground`. An invalid expression switches it to `text-destructive`.
- The raw five-field expression and the labels `text-muted-foreground`, `text-xs`, in the mono font for the expression.
- State badge uses the status badge tokens: `muted` for paused, `ok` for active.
- Card border `border-border`, radius `rounded-none`, padding `p-md`.

## States

- **active:** the badge reads "Active". The next run shows its time in the schedule's time zone.
- **paused:** the badge reads "Paused". The next run reads "Paused", so nobody reads a stale time as a promise.
- **invalid expression:** the plain-words line shows the reason in `text-destructive`. The raw expression stays visible under it.
- **no name:** the heading reads "Untitled schedule".

## Keyboard and screen reader

- The name is a level-two heading, so a list of cards is navigable by heading.
- The badge says "Active" or "Paused" in words. Colour is never the only signal.
- Where the card is a link, the whole card is the link target. Its text is the schedule's name and next run, read in order.
- The labels (Agent, Time zone, Next run) are a definition list, so each value is announced with its label.

## Touch

- The card is a full-width block with at least 44 px of height, so the whole card is the tap target.
- Long agent names and expressions wrap or break. Nothing scrolls sideways at 390 px.

## Behaviour that must hold

- The plain-words line comes from the same rule the form uses, so the card and the form never disagree about what an expression means.
- A paused schedule never shows a next-run time.
- Times are shown in the schedule's own time zone, with the zone named beside them.
- The card does not fetch data. Its caller passes every value in.

## Do

- Show the raw expression under the plain-words line, so the owner can check it.
- Keep the agent as `namespace/name`, the same form the rest of the app uses.

## Don't

- Don't colour the state alone. The word carries the meaning.
- Don't show a next-run time for a paused schedule.
- Don't truncate the name with an ellipsis; wrap it.
