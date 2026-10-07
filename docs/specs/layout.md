# Layout

Mainloop is responsive across mobile and desktop viewports.

Project pages load repository details before showing project controls. If that request fails,
the page shows the load error and a **Retry loading project** button instead of remaining on
the loading message. Retrying requests the details for the current project.

## Desktop

- Chat takes main area
- Sessions sidebar always visible on the right; the inbox and projects below it size to their content. The projects section also holds the **New workspace** control (repository, optional branch).
- No tab bar

## Mobile

- Bottom tab bar with Chat, Sessions and Inbox tabs (the Sessions tab includes the "+ agent" link)
- Chat tab active by default on load
- Tab bar hidden on desktop viewports
- Touch targets sized appropriately for mobile interaction
- Tabs switch between the Chat, Sessions and Inbox views

## Tasks

Durable tasks are read from the owner task API (see [tasks.md](tasks.md)). The page shows what the
server reports; it never decides a task's state locally.

- **Project page**: a **Tasks** section lists the project's tasks as a tree, supervisors with their child tasks beneath them. A child whose parent is not loaded is still listed. Each row shows the status and reason, the active provider, any operation in progress, and CI (current CI states require both head SHAs present and equal and a valid observation no older than ten minutes; otherwise unknown).
- **Task page** (`/tasks/<id>`): the task and its parents and children; the active provider and native session; the workspace, environment version and branch; the attempt history, including superseded attempts as read-only history; PR, CI and merge as three separate facts, each shown as unknown when the server has not reported it, with how long ago they were observed (flagged stale after ten minutes); the step and last confirmed step of any open provider switch or other operation; and the provider's own summary, labelled **Provider summary; unverified**. A report or finished turn is never shown as proof the task is complete. PR links open only when they are `https` URLs, in a new tab with `rel="noopener noreferrer"`.
- Failed task reads, including background updates and reconnects, show a last-known-state warning and retry control on the task page. The warning clears after a successful read of that task; list failures are tracked separately.
- **Actions**: **Retry**, **Reassign** (to the other provider, chosen from the profiles that offer the attempt's role) and **Cancel task**. Each is enabled only when the server's eligibility says so, and sends the task version and attempt shown on the page. If the task changed first, the server refuses with a conflict and the page re-reads the task. If a response is lost, a dedicated **Resend original request** control shows the original target, version and attempt and reuses the same request. It remains available when eligibility or the current provider changes, and is disabled while a request is in flight. The notice says the outcome is unconfirmed. Pending approvals and a required checkpoint are listed as blockers.
- **Updates**: `task:updated` events only prompt a re-read; duplicates are ignored, and after the event stream reconnects the page re-reads every task it holds.
- **Links**: the session and workspace pages link to the task that owns them (noting when the attempt is superseded), and an Inbox approval card links to its task. Approvals are still answered in the Inbox; the task page only points to it.
- **Mobile**: the same sections in a single column. Buttons and the provider picker are at least 44px tall.
