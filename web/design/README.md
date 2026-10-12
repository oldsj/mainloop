# Design tokens

This folder holds Mainloop's visual source of truth. Values are defined once, in `tokens.json`, and everything else is generated from it.

## Workflow

1. **Edit `tokens.json`** (W3C Design Tokens format). It is the only place a value is defined.
2. **Run the build** from the repository root: `node web/design/build.ts`. It needs Node 24 and no installs.
3. **Commit `tokens.json` and the generated files together.**
4. **The app and the design artifact read the output.**

The build writes two things:

- `web/src/styles/theme.css`: the Tailwind v4 `@theme` block, and one variable set per theme. The new app imports it.
- `web/design/system/`: the files for the published Claude Design System artifact (`README.md`, resolved `tokens.json`, and `guidelines/`). Do not edit it by hand.

## Components use tokens only

Components in `web/` use token-backed utilities such as `bg-card`, `text-muted-foreground`, `border-border`, `text-sm`, `p-md` and `rounded-none`.

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
and printing moderate/minor findings. Chromium is cached in CI; no backend or live
agent is used. Install it locally with `pnpm --dir web exec playwright install chromium`.
