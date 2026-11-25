# Design tokens

This folder holds Mainloop's visual source of truth. Values are defined once, in `tokens.json`, and everything else is generated from it.

## Workflow

1. **Edit `tokens.json`** (W3C Design Tokens format). It is the only place a value is defined.
2. **Run the build** from the repository root: `node web/design/build.ts`. It needs Node 26 and no installs.
3. **Commit `tokens.json` and the generated files together.**
4. **The app and the design artifact read the output.**

The build writes two things:

- `web/src/styles/theme.css`: the Tailwind v4 `@theme` block, and one variable set per theme. The new app imports it.
- `web/design/system/`: the files for the published Claude Design System artifact (`README.md`, resolved `tokens.json`, and `guidelines/`). Do not edit it by hand.

## Components use tokens only

Components in `web/` use token-backed utilities such as `bg-card`, `text-muted-foreground`, `border-border-subtle`, `text-body`, `p-md`, `min-h-control` and `rounded-none`.

Do not put these in a component:

- hex colours, `rgb()` or `oklch()` values;
- pixel or rem values for spacing, type or radius;
- Tailwind palette names such as `neutral-50` or `red-500`.

If a component needs a value that does not exist, change `tokens.json` first, then rebuild.

## Themes

Five themes are defined: `eighties` (the default), `dracula`, `nord`, `gruvbox` and `tokyo-night`.

- The default theme lives on `:root`. Other themes use `[data-theme="<name>"]`, set on `<html>`, the same element as `:root`.
- Each theme defines its terminal palette (`term-*`). The shadcn-style semantic names (`background`, `primary`, `muted`, and so on) are redeclared in every theme as `var(--term-*)` aliases. That is what lets a theme switch change every colour at once.

To add a theme, add `color.theme.<name>` with every `term` and `semantic` key. The build fails if any key is missing. Then add the theme to the app's theme list, which is outside this folder.

## Adding or changing a token

- Put the token under the matching group. Use `{group.path}` to alias another token.
- Aliases must point to a token of the same type.
- Colours are 6-digit hex. Lengths include a unit (`0.25rem`). Durations include a unit (`200ms`).
- Values are DTCG strings with units, not the 2025.10 object form.

## What the build checks

The build exits `1` and writes nothing if any of these fail:

- `tokens.json` parses as JSON;
- every token has `$value` and a known `$type` (`color`, `dimension`, `duration`, `fontFamily` or `cubicBezier`);
- every alias resolves, to a token of the same type, with no cycles;
- every colour and length is well formed;
- every theme has the full set of `term` and `semantic` keys;
- `web/design/components/` exists and holds guideline files.

## Determinism

The output has no timestamps and keeps a fixed order. Running the build twice leaves the tree unchanged. The generated files are already in the format `trunk fmt` would produce, so formatting does not change them.

CI (`.github/workflows/web.yml`) regenerates the files and fails on any diff.

## Type

Two families, five size roles plus a field size, and three weights. They are tokens, and they are the only ones there are: the build clears Tailwind's own families, sizes and weights (`--font-*`, `--text-*` and `--font-weight-*: initial`), so `text-sm`, `font-bold` and `font-sans` produce nothing. `design/type.test.js` fails on any of them under `src/`, and on an arbitrary `text-[…]`.

- `font.family.sans`, the system UI font, is the page default (`body` sets it). The chrome, headings, buttons, forms, rows and the transcript's prose use it.
- `font.family.mono` is opt-in through `font-mono`, for text that is code or that a machine reads: code spans and blocks, paths, branch names, shas, PR numbers, cron lines, globs, image digests, tool calls and their output, terminal blocks and the composer's footer row. It is SF Mono where the browser has it; everywhere else Geist Mono, one variable woff2 the app serves itself from `src/assets/fonts`, under the SIL OFL 1.1. Do not use mono to make text look technical.
- Sizes, each with its own line height. A size with two values is one variable, like the control size: the touch value by default and the pointer value at 1024 px and wider with a mouse or trackpad.

  | Role           | Pointer | Touch | Use                                                                                          |
  | -------------- | ------- | ----- | -------------------------------------------------------------------------------------------- |
  | `text-label`   | 11 px   | 11 px | Section headings, upper case, semibold, `tracking-wide`. Nothing else is upper case.         |
  | `text-caption` | 12 px   | 12 px | A row's second line, ages, metadata, the status bar, help under a field.                     |
  | `text-body`    | 13 px   | 15 px | The default: rows, tabs, buttons, prose.                                                     |
  | `text-field`   | 13 px   | 16 px | Text the owner types: the composer and every field. 16 px stops a phone zooming in on focus. |
  | `text-title`   | 15 px   | 17 px | A thread's, section's or dialog's name. Semibold.                                            |
  | `text-display` | 20 px   | 20 px | The page's one `h1`. Semibold.                                                               |

- Weights: `font-normal` (400), `font-medium` (500) for row titles and emphasis in the chrome, `font-semibold` (600) for headings and titles.
- A size is never named after a colour (`text-input` would set both the size and `--color-input`); a test checks this. A size added to the tokens must also be added to `TOKEN_FONT_SIZES` in `src/components/ui/cn.ts`, or `cn` treats it as a text colour and drops it; a test checks this too.

## Control size

`size.control` sets the minimum height of every button, field, link target and menu row, and the width of an icon button. It has two values and one variable, `--spacing-control`:

- `touch`, 44 px, is the default. It applies on touch screens and on every screen narrower than the desktop layout.
- `pointer`, 28 px, applies at 1024 px and wider when the primary pointer is a mouse or trackpad (`(min-width: 1024px) and (pointer: fine)`).

Components write `min-h-control`, `w-control` or `min-w-control`, never `min-h-11` or a breakpoint pair such as `min-h-11 lg:min-h-7`. A guideline that says a target is "at least 44 px" describes the touch value. The phone tab bar is the one exception: it is always 44 px.

## Borders

There are two border colours.

- `border` (and `input`) is at least 3:1 against the background. It marks the edge of a field: a text input, a select, a checkbox, or a control whose edge is its only cue.
- `border-subtle` is the quiet one. It is the rule between regions and rows, and the outline of a secondary (`outline`) button. It shows on `bg` and `bg-secondary` but stays well under 3:1, so it never competes with content.

Write `border-border-subtle` for a divider. Bare `border` still resolves to `border`, so a new field is visible by default.

## Status colours

A row's state is one dot, in the same language as herdr's agent list, so the two read alike side by side. Components use these tokens, not the raw palette, for a status dot, badge or word.

| Token              | Dot          | Meaning                                                                                                        | Palette colour                 |
| ------------------ | ------------ | -------------------------------------------------------------------------------------------------------------- | ------------------------------ |
| `status-working`   | filled amber | working: a turn or task is running, or waiting on CI or another task                                           | `term-yellow`                  |
| `status-attention` | filled red   | **Needs you**: a question, an approval, or a failure that needs a decision. The word beside the dot says which | `term-error`                   |
| `status-failed`    | filled red   | a failure; drawn as Needs you                                                                                  | `term-error`                   |
| `status-unseen`    | filled teal  | finished and not yet looked at                                                                                 | `term-cyan`                    |
| `status-idle`      | hollow       | at rest: idle, asleep, or finished and seen                                                                    | `term-fg-muted`, as an outline |

- The label for red is always "Needs you", as in the inbox. Never "blocked".
- At rest is drawn hollow (`border border-status-idle`, no fill), so a list of quiet rows stays quiet.
- The accent blue is for focus, selection and links only. It is not a status.
- `term-green` is for facts that are good, not for a row's state: a healthy connection, passing CI, a merged PR, diff additions and success notices.
- Warning paragraphs still use `term-yellow` directly; that is text, not a status.
- "Seen" is the owner's, kept on the server, so it follows them between devices (see `docs/specs/seen.md`).

## Radius

Every radius token is `0`: `radius.xs` to `radius.4xl` all alias `radius.default`, so utilities such as `rounded-lg` and `rounded-3xl` resolve to square corners. Tailwind v4 hard-codes bare `rounded` and `rounded-full`, which no token can override. The `tokens/tokens-only` ESLint rule in `web/eslint-rules/` therefore forbids every radius class except `rounded-none` (and the per-side `rounded-t-none` forms). Components use `rounded-none`.

## Component guidelines

`components/` holds one guideline per product component: composer, message, delivery receipt, attention card, HITL card, session summary, evidence link and workspace action. Each covers purpose, states (empty, loading, stale, error, offline), keyboard and screen-reader behaviour, 44 px touch targets, and do and don't. The build copies them into `system/guidelines/`.

The behaviour comes from the specs in [`docs/specs/`](../../docs/specs/). Where a guideline adds a rule the specs do not state, it says "proposed".

## Open items

- Some tokens are proposed and not yet in `packages/ui`: the `motion.duration` values `fast` and `base`, the `motion.easing` value `standard`, and the focus ring width and offset. Their `$description` says so.
- Font weights and `max-width` are not tokenised. The old `max-width` values had no effect in the current app.

## Preview build and accessibility

Run `pnpm --dir web build:design` to generate the theme and build every
`src/**/*.preview.tsx` into `system/previews/`, with an index. The design build
uses a separate Vite root and output directory; the app still builds into `web/dist`.
The pages load interactive React fixtures and the same generated theme as the app.

Previews are generated in CI and ignored by Git: committing hashed bundles would
create churn without adding useful drift evidence. Tokens, theme and guidelines
remain committed and drift-checked. Build previews after generating the tokens,
because the token generator replaces `system/`.

Run `pnpm --dir web check:design` after building. It serves the built pages locally
and checks each with axe in Chromium, failing on serious or critical violations
and printing moderate/minor findings. It also loads each page 390 px wide (a phone) and fails
when anything runs past the preview's edge, outside a scroll or clipping container: touch sizes
are larger and mono runs wide, so a row that fits on a desktop can push a control off-screen. Give a component whose row can get long a preview with its longest labels. Chromium is cached in CI; no backend or live
agent is used. Install it locally with `pnpm --dir web exec playwright install chromium`.
