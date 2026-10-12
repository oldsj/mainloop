# Environment selection

Source: `docs/specs/environments.md` (implemented project UI).

## Purpose

The environment section on a project page. The owner chooses which statically validated environment version new sessions in this project use, or follows the environment's accepted default. The same section registers a public, digest-pinned image. Changes apply to newly started sessions only.

## Tokens

- Section `border-border`, padding `p-md`, gap `gap-md`, radius `rounded-none`.
- Heading `text-lg font-medium`. Saved and draft lines `text-sm`. Labels and explanations `text-muted-foreground`.
- Warning for the accepted default `text-term-yellow`. Save error `text-destructive`.
- Selects and the image input `border-border bg-background`. Save and register are primary buttons. Accept is an outline button.

## States

- **saved line:** "Saved: <environment> · Follow accepted default", or "Saved: <environment> · Fixed version · <version>", or "… · No accepted default". It shows "· Access revoked" when the owner has lost access. It stays separate from the draft.
- **draft:** the owner's choice, edited without changing the saved line.
- **version labels:** an eligible version reads "<id> · <platform> · Statically validated". A pending or obsolete version reads "<id> · <platform> · Unavailable: pending build or obsolete policy" and cannot be chosen.
- **follow:** "Follow accepted default" needs an eligible accepted default. Without one, the owner must choose a version and accept it first.
- **accept as default:** the button reads "Accepting…" while busy. The warning "This changes the default for every project following this environment." sits beside it.
- **saving:** "Saving…" and disabled. Success says "Environment selection saved for new sessions."
- **conflict:** "<server message>. Review the refreshed saved selection before saving again." The draft is kept, and the saved selection is re-read.
- **version list unreadable:** with no versions loaded, "Couldn't load the versions for this environment." then the reason, with "Retry loading versions". With versions already loaded, "Couldn't refresh the versions for this environment." then the reason and "Showing the last loaded versions." While a read is in flight with nothing fresh, "Loading versions…".
- **defaults warning:** "An accepted current-policy version is required…" shows only after a fresh version read. An empty or failed list does not trigger it.
- **registration:** "Register and validate image". The notice after a success reads "Registered <name>: <status>, <validator>. Runtime capabilities have not been tested."
- **uncertain registration:** "Registration reply was not confirmed. Refresh the list and reconcile it before…". Registering again is blocked until the owner refreshes.
- **saved selection unreadable:** with no saved selection loaded, "Couldn't load the environment" with "Retry loading environment". With one loaded, "Couldn't refresh the environment" sits above it and says it is showing the last saved selection.
- **environment list unreadable:** "Couldn't load environments" with "Retry loading environments". A refresh failure keeps the list and says "Couldn't refresh environments".

## Keyboard and screen reader

- Every control has a visible label: "Choose an environment", "Choose a version", "Image", the architecture select. Options read in full, including the unavailable reason.
- The image input shows the expected shape as placeholder text, `registry/repository@sha256:digest`. Its format is also checked on submit.
- Errors and conflicts are alerts. An uncertain registration is announced once.

## Touch

- Every select, input, checkbox, button and the refresh control is at least 44 px tall.

## Behaviour that must hold

- Reads never select a version or accept a default.
- Only a statically validated version from the current policy can be saved or accepted.
- Static validation is metadata evidence. The UI never says a version is ready to run.
- An uncertain registration keeps the form locked until the owner refreshes.
- A conflict keeps the owner's draft for review. It is not replaced silently.
- A successful registration resets the form, so the same image cannot be submitted again by accident.
- A failed save, accept or registration clears the success notice from earlier, so the screen never shows both.

## Do

- Show the saved choice and the draft apart, so the owner sees what is live and what they are about to change.
- Name the effect of accepting a default on every project that follows the environment.

## Don't

- Don't auto-select the newest version, or the default, when the owner opens the section.
- Don't present "Statically validated" as "Ready" or "Runs".
- Don't use palette colours or hex values. Tokens only.
