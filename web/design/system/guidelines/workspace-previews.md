# Workspace previews

Source: `docs/specs/workspaces.md` (Preview and idle-out).

## Purpose

Links to a workspace's declared HTTP ports, so the owner can open a running app in a new tab. Opening the preview of a suspended workspace wakes it, and the copy says so.

## Tokens

- Each link is a row with a `border-border` bottom rule and `py-sm`.
- The link is at least 44 px tall (`min-h-11`), with `text-foreground` and an underline.
- The port number uses `text-muted-foreground font-mono`.
- Notes use `text-muted-foreground text-xs`. Failures use the shared error recovery block.

## Content

- Only `http` and `https` URLs are offered. The URL is the server's, used as given. The client never builds a host.
- Each link opens in a new tab with `rel="noopener noreferrer"`. Its accessible name names the port: "Open preview web, port 3000".
- A row with a bad name, port or URL is left out and counted: "One preview could not be read and is not listed."

## States

- **Loading:** "Loading previews…", announced as a status.
- **None declared:** "No preview ports are declared."
- **Failed refresh with links already shown:** the links stay. The error block says the list may be out of date.
- **Failed first load:** "Preview ports could not be loaded." with Try again.
- **Unreadable list:** "Mainloop sent preview ports this page cannot read."

## Do

- Say that opening a suspended workspace's preview wakes it.

## Don't

- Don't offer a link for a `javascript:` or other non-web URL.
- Don't put a hostname in the client. Use the URL the server sent.
