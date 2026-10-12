# Workspace manifest

Source: `docs/specs/workspaces.md`.

## Purpose

The facts a workspace was declared with: repository, branch, base ref, agent, development environment, preview ports and idle timeout. It is read-only. Nothing in this section changes the workspace; the lifecycle controls do that.

## Tokens

- A definition list. Labels use `text-muted-foreground`; values use `text-foreground`. Values wrap with `break-words`.
- Stacked on phones. From `sm` the label takes a fixed column (`sm:w-44`).
- Radius `rounded-none`.

## Content

- **Repository:** a link only when the URL is `http` or `https`. It opens in a new tab with `rel="noopener noreferrer"`. Any other value is shown as plain text.
- **Development environment:** the short digest and the platform, for example `sha256:0123456789ab · linux/amd64`. The full image reference is in the element's title. A digest that does not parse shows "unknown", never an error.
- **Preview ports:** "name: number" for each declared port, or "None declared".
- **Base ref:** "Default" when the manifest's ref is empty.
- **Idle timeout:** whole minutes.

## States

- **Declared:** every fact is shown.
- **No development environment:** the row reads "Not declared".
- **Unreadable port:** left out. The note says how many were not listed ("One declared port could not be read and is not listed.").

## Do

- Show what the manifest declares. The lifecycle badge shows what the runtime is doing.

## Don't

- Don't link a repository URL that is not `http` or `https`.
- Don't show a malformed digest as text. Show "unknown".
