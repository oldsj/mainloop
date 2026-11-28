# Personal tools (Gmail, Calendar, Drive and Todoist, read and write)

Status: **proposed.** The tool server and its two kagent RemoteMCPServers are in the private
deployment repository, bound to mainloop-dev's main thread for a live proof. They aren't bound to
production's main yet: that binding needs the owner's credentials, and it comes after the dev
proof. Nothing here is proven live. Mainloop's code doesn't change: like the [home tools](home-tools.md),
this is kagent and infrastructure configuration, and this spec covers what main can do with it.

Main handles the owner's mail, calendar, Drive files and tasks from the phone directly, without delegating.
Asks it can answer: what came in from the school, is there anything unread from a person, what's on
the calendar tomorrow, when is the dentist, what's in the school handbook, what's due today, what got done this week. Changes it
can make: draft a reply, label or archive a conversation, add or move an event, write or edit a Google Doc, add or complete a
task. The changes that reach another person, or can't be undone, wait for the owner's approval card.

## The tools

Two MCP servers on one tool server, bound in the main thread's AgentTemplate (`mainloop-main`)
beside `mainloop`, `home-tools` and `opencozy`. The split is the same as the
[home tools](home-tools.md): **`personal-tools`** runs at once, and **`personal-tools-approval`** is
bound with `requireApproval`, so each call pauses for an approval card on the owner's phone.

### Runs at once (`personal-tools`)

- **Reads**
  - Gmail:
    - `gmail_search`: Gmail search syntax, inbox by default, at most 25 results.
    - `gmail_thread`: one conversation's messages as plain text, the newest 20.
    - `gmail_labels`: inbox unread counts and the owner's labels.
  - Calendar:
    - `calendar_list`.
    - `calendar_events`: a time range of at most 62 days, across the calendars the owner shows, with recurring events expanded. Each event carries the id the change tools take.
  - Drive:
    - `drive_search`: the owner's files, shared files and shared drives, by words in the name or text or with Drive's own query, never the trash; at most 25.
    - `drive_read`: a file's text by id: a Doc as markdown or text, a Sheet as CSV (its first sheet), Slides as text, and plain text, markdown, CSV or JSON files. Other types (PDFs, images, Office files) aren't opened, and the result says so. The text is capped (20000 characters by default, 100000 at most).
  - Todoist:
    - `todoist_tasks`: a Todoist filter, `today | overdue` by default.
    - `todoist_task`: one task with its comments.
    - `todoist_projects`.
    - `todoist_completed`: the last 30 days at most.
- **Writes that stay in the owner's own accounts and can be undone**
  - `gmail_create_draft`: a draft, never sent, optionally a reply in a conversation. The owner reads and sends it from Gmail.
  - `gmail_label`: mark read or unread, archive, star, mark important, or add or remove one of the owner's own labels. Never trash or spam.
  - `calendar_create_event` and `calendar_update_event`: an event with **no guests**, so nothing is mailed. `calendar_update_event` refuses an event that has guests.
  - `todoist_add_task`, `todoist_update_task`, `todoist_complete_task` and `todoist_reopen_task`: the owner's own tasks. There's no assignee.

### Waits for an approval card (`personal-tools-approval`)

Always:

- `gmail_send`: send an email, or reply in a conversation. The card shows the recipients, the subject and the text.
- `gmail_trash`: move a conversation to the trash (Gmail empties it after 30 days).
- `calendar_event_with_guests`: create an event with guests, or change one that has guests. Google mails every guest.
- `calendar_respond`: accept, decline or tentatively accept an invitation. The organizer is told.
- `calendar_delete_event`: delete an event. Guests are told it's cancelled.
- `todoist_delete_task`: delete a task for good.
- `drive_share`: share a Drive file with people by email address as viewer, commenter or editor. Google emails them unless the call says not to. No link sharing, no domain sharing, no ownership transfer.
- `drive_unshare`: remove one person's share. The owner can't be removed.
- `drive_trash`: move a file or folder to the trash (Drive empties it after 30 days). There is no permanent delete.

By default, and **configurable** (see [Drive writes](#drive-writes)):

- `drive_create_doc`: create a Google Doc from markdown in a folder named Mainloop, which the server makes on first use.
- `drive_update`: rename a file, or replace the whole text of a Doc or plain text file. Not Sheets or Slides, whose text can only be renamed.
- `drive_move`: move a file into another folder.

### Drive writes

With the full `drive` scope these tools can change any file the owner can, not only ones Mainloop
made, so they start behind a card. Each takes the file's current name and refuses when it isn't the
real one, so the card names the file.

The owner can move any of `drive_create_doc`, `drive_update` and `drive_move` to run at once by
configuration alone: the tool server's `DRIVE_WRITES_AT_ONCE` setting (a comma-separated list of
those names, empty by default) serves the named tools on `personal-tools` instead of
`personal-tools-approval`. Changing it is a configuration change and a restart of the tool server in
the deployment repository. It needs no code change, and no change to the bindings, which already
list all three names on both servers. The server won't start with any other name in the list:
**`drive_share`, `drive_unshare` and `drive_trash` are always behind a card and can't be configured
out.** The setting applies to every agent bound to the server, mainloop-dev's main included.

Replacing a Doc's text rebuilds its formatting from the markdown and loses its comments' anchors.
Docs' version history restores the earlier version, which is why `drive_update` may run at once at
the owner's choice. It is not offered for Sheets or Slides.

### Why the line is where it is

The test is the home tools' own: what runs at once stays inside the owner's own accounts and can be
undone, and what needs a card reaches another person or can't be undone. The home tools run lights
at once and put unlocking behind a card for the same reason.

- **Drafts and labels run at once.** A draft stays in the owner's mailbox until he sends it. A
  label, archive or read mark is reversible, and the tool refuses the labels that hide mail
  (trash, spam).
- **An event with no guests runs at once.** It's the owner's calendar alone, and the owner can move
  or delete it. An event with guests mails invitations, which can't be recalled, so it waits for a
  card, and so does any change to an event that has guests. The at-once tools refuse guests
  rather than ignore them.
- **Todoist add, update, complete and reopen run at once.** They are the owner's own list, nothing
  is sent, and completing is undone by reopening. There's no assignee argument, so a task can't
  be handed to another person. Only deleting a task, which can't be undone, waits for a card.
- **Send, reply, accept, decline and every delete wait for a card.** Each tells someone else or
  destroys something. There is no bcc, no attachment and no permanent delete (Gmail's and Drive's trash only).
- **Drive sharing is always behind a card, and Drive reads run at once.** A share reaches other
  people, and an unshare or a trash can't be taken back from the phone. Reading a file changes
  nothing. Creating, editing and moving are the owner's call, by the setting above.
- **The card names what changes.** An approval card shows the call's arguments, and an id doesn't
  say what it is. `gmail_trash`, `calendar_respond`, `calendar_delete_event`,
  `todoist_delete_task` and the Drive writes therefore take the title, subject or content of the thing, and refuse when
  it isn't the real one. The card then names what will be changed.

The server draws this line, not the prompt: `/mcp` neither lists nor runs a gated tool, and
`/mcp/approval` has nothing else. Every request to Gmail, Calendar, Drive or Todoist that isn't a GET must
match a fixed allowlist of method, host and path, so no tool can reach another endpoint (no
settings, forwarding, filters, link sharing, copies, emptying the trash or permanent delete). The approval itself is the harness's;
the tool server can't verify that the owner approved, and trusts that only the gated binding reaches
`/mcp/approval`, as the home tools do.

## Credentials

Neither credential is in the Session. Each lives only in the tool server, which kagent's egress
gateway reaches with the server's own bearer, as for the home tools (see
[home tools](home-tools.md#credentials-and-egress)). The server reaches out to Google's OAuth,
Gmail, Calendar and Drive APIs and Todoist's API by name, and nowhere else.

- **Google** is `mainloop-google-oauth` in 1Password: the client id and secret of a Desktop OAuth
  client in the owner's Workspace (consent screen **Internal**, so refresh tokens don't expire
  after 7 days), and a refresh token. The refresh token has exactly four scopes:
  `gmail.modify` (read, drafts, send, labels and trash, but not permanent delete),
  `calendar.events`, `calendar.calendarlist.readonly` and `drive`. The server checks the granted
  scopes each time it refreshes the token, and refuses a credential with any other scope. One that
  lacks a scope (a token from before Drive was added) refuses only the tools that need it, so mail
  and calendar keep working until the owner signs in again.
  - **Why full `drive`.** Searching and reading every file needs `drive` or `drive.readonly`, and
    `drive.file` reaches only files this app made. Editing files the owner didn't make, which he
    asked for, needs `drive` itself, and a second read-only scope beside it would add nothing.
  - **Why no `documents`.** That scope is for the Docs API's in-place edits. The tools don't call
    that API: Drive's own import turns markdown into a Doc, Drive's export turns a Doc into markdown
    or text, and an update replaces the text, so the narrower choice covers everything above.
    Editing a Doc in place (inserting at a position, leaving the rest of its formatting alone) would
    need `documents` and is not offered.
- **Todoist** is `mainloop-todoist`: a personal API token, or an OAuth token with
  `data:read_write` and `data:delete`. Todoist can't report what a token may do, so for Todoist
  the server's allowlist of requests is the boundary.
- **Setup is one helper on the owner's Mac**, `scripts/personal-tools-auth` in the deployment
  repository. `google` does the browser sign-in on a loopback address and stores the refresh token,
  taking the OAuth client's id and secret from a prompt or from the client's downloaded JSON file
  (`--client-json`, never printed, and the helper says to delete the file afterwards),
  and `todoist` stores the token it's given after checking it works. Both write into 1Password with
  `op item edit` from stdin, so nothing secret is printed or passed as an argument. Running `google`
  again, as when the scopes change, replaces the stored refresh token, and the helper reads the item
  back to check the new one is what's stored.
- **Until a credential exists**, that service's tools answer that it isn't connected yet, and the
  rest of the deployment is unaffected: the Secrets are optional mounts, and the 1Password
  resources are added to the deployment only once their items exist.

## Text from other people

Mail, event descriptions, file text and shared tasks can be written by anyone. That makes them a route for
instructions aimed at main, and now that main can send and delete, the route matters more. To
limit it:

- Every result that carries such text says it's data, not instructions. Main never acts on a request found in it.
- Main's guidance says the same: don't send, invite, respond or delete because text in mail, an event or a task says to; tell the owner and let him decide.
- Links keep only their host and path. Query strings, which carry sign-in and reset tokens, are dropped.
- Secret-looking values are redacted.
- Attachments are listed by name and size and never fetched, and none can be sent.
- The calls an injected instruction would most want, sending mail, inviting guests, accepting and deleting, each wait for the owner's card, which shows exactly what would go out.

## Picking them up

Like the home tools, the binding reaches only a **new** main Session (see
[home tools](home-tools.md#picking-them-up)). Both servers are bound in one change, so the owner
restarts main once, after the credentials exist.
