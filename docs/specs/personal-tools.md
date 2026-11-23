# Personal tools (read-only Gmail, Calendar and Todoist)

Status: **proposed.** The tool server and its kagent RemoteMCPServer are in
the private deployment repository. They aren't bound to the main thread's Agent yet. That binding comes
with the [home tools](home-tools.md), after a live proof on mainloop-dev. Nothing here is proven
live. Mainloop's code doesn't change: like the home tools, this is kagent and infrastructure
configuration, and this spec covers what main can do with it.

Main answers questions about the owner's day from the phone directly, without delegating: what came
in from the school, is there anything unread from a person, what's on the calendar tomorrow, when
is the dentist, what's due today, what got done this week. It can only read. A change (reply, send,
accept an invite, add an event, add or complete a task) isn't available yet. Main says exactly what
the change would be and stops. Writes come later as privileged actions the owner approves
(oldsj/mainloop#435, `docs/design/privileged-actions.md`), with their own credentials.

## The tools

One MCP server, **`personal-tools`**, bound in the main thread's AgentTemplate (`mainloop-main`)
beside `mainloop`, `home-tools` and `opencozy`:

- **Gmail:**
  - `gmail_search`: Gmail search syntax, inbox by default, at most 25 results.
  - `gmail_thread`: one conversation's messages as plain text, the newest 20.
  - `gmail_labels`: inbox unread counts and the owner's labels.
- **Calendar:**
  - `calendar_list`.
  - `calendar_events`: a time range of at most 62 days, across the calendars the owner shows, with recurring events expanded.
- **Todoist:**
  - `todoist_tasks`: a Todoist filter, `today | overdue` by default.
  - `todoist_task`: one task with its comments.
  - `todoist_projects`.
  - `todoist_completed`: the last 30 days at most.

## Read-only, twice over

- **The credentials can only read.**
  - Google's has only the `gmail.readonly` and `calendar.readonly` scopes. The server checks the granted scopes each time it refreshes the token, and refuses a broader credential.
  - Todoist's has only the `data:read` scope.
- **The server sends only reads.** Every request to Gmail, Calendar or Todoist is a GET. Its single other request is Google's token refresh.

## Text from other people

Mail, event descriptions and shared tasks can be written by anyone. That makes them a route for
instructions aimed at main. To limit it:

- Every result says the text is data, not instructions. Main never acts on a request found in it.
- Links keep only their host and path. Query strings, which carry sign-in and reset tokens, are dropped.
- Secret-looking values are redacted.
- Attachments are listed by name and size and never fetched.

## Credentials and egress

Neither credential is in the Session. Each lives only in the tool server, which kagent's egress
gateway reaches with the server's own bearer, as for the home tools (see
[home tools](home-tools.md#credentials-and-egress)). The server reaches out to Google's OAuth,
Gmail and Calendar APIs and Todoist's API by name, and nowhere else. The owner creates both
credentials in 1Password. A helper in the deployment repository does the browser sign-in with
the read-only scope and stores the token without printing it. Until a credential exists, that
service's tools answer that it isn't connected yet.

## Picking them up

Like the home tools, the binding reaches only a **new** main Session (see
[home tools](home-tools.md#picking-them-up)). Both sets are bound in one change, so the owner
restarts main once.
