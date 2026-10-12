# Layout

Mainloop is responsive across mobile and desktop viewports.

Project pages load repository details before showing project controls. If that request fails,
the page shows the load error and a **Retry loading project** button instead of remaining on
the loading message. Retrying requests the details for the current project.

## Desktop

- Chat takes main area
- Sessions sidebar always visible on the right; the inbox and projects below it size to their content. The projects section also holds the **New workspace** control (repository, optional branch).
- Sessions default to current work. **Show finished (N)** reveals history without clearing it;
  available idle sessions have their own **Show idle (N)** filter.
- The inbox defaults to **Needs you** with a badge for unresolved owner actions. Its **Activity**
  tab holds quieter FYIs and unverified provider reports. Empty attention says **Nothing needs you**.
- The header has a **health** link to orchestration health, beside **new agent session**.
- No tab bar

## Mobile

- The header has an icon link to orchestration health, labelled "Orchestration health", beside the new-session icon.
- Bottom tab bar with Chat, Sessions, Inbox and Projects tabs (the Sessions tab includes the "+ agent" link). It has at most five tabs, each labelled in one short word. The four tabs are, in order: Chat, Sessions, Inbox, Projects.
- Chat tab active by default on load
- Tab bar hidden on desktop viewports
- Touch targets sized appropriately for mobile interaction
- Tabs switch between the Chat, Sessions, Inbox and Projects views
- A session bound to a task is listed under the task's title, with the repository as secondary text. Other sessions keep their own title.
- Sessions use the same current/history filters as desktop. Inbox contains **Needs you** and
  **Activity**; the Inbox badge excludes Activity. Filter and inbox-view controls are at least
  44px tall and wrap within the available width.
- Switching tabs keeps what each tab holds, such as a draft or a loaded list. A tab is built the
  first time it opens.
- The header and tab bar stay clear of the device's safe areas (notch, rounded corners and the
  home indicator).

## Shell

- The offline banner is one line at the top of the screen. It takes its height from the page, so
  the composer and the tab bar stay on screen at 390 × 844. Long text is cut off with an ellipsis.
- Each page sets the document title, for example **Health · Mainloop**.
- A route change moves keyboard focus to the main landmark, so focus does not fall back to the
  page body.

## Home

- **Work list**: tasks grouped as **Needs you**, **Working**, **Blocked or failed**, and **Finished since you last looked**. A task appears once, in the first group that fits. Each row shows the title, project, a one-line status with its reason, the relative last update time, and links to the task. Running and queued tasks are current work, so **Working** always shows; with nothing running it reads **Nothing running. Start work from the main chat.** Each group shows five rows, then a **Show all (N)** toggle that becomes **Show fewer**, so a long list does not push Sessions and Inbox out of reach.
- **Since you were here**: counts of tasks waiting on you, failed, and merged since the last visit, each linking to the most recent one. It is hidden when nothing changed, and on a first visit, which has no baseline. On the phone it is one line that scrolls sideways rather than wrapping, and it shows only on the main chat (`/`), not above agent or task pages.
- **Last visit**: kept per browser in local storage. The app shell reads it once when it opens and records the current time, so the desktop aside and the phone tabs share one baseline. Because the desktop aside shows on every route, the visit is the app's last open, not only the last time home was shown. This is deliberate: the baseline is the last app open, not the last home view. If storage is blocked, the strip and the finished group simply have no baseline.
- **Updates**: the list refreshes from the same task invalidation as the task pages. The relative times refresh each minute on the page.
- **Placement**: on desktop the work list and the strip sit at the top of the right-hand aside, above sessions, inbox and projects; the centre column is the chat alone. On the phone the strip sits above the main chat on the Chat tab, and the work list sits first in the Projects tab, above the project list. The tab count stays at four.
- **Not proven**: the strip with content in a browser. Phone (390×844) and desktop (1440×900) layouts were checked in a browser against fixtures with no tasks, so only the empty states were seen; the strip and list rows are covered by unit tests.

## Tasks

Durable tasks are read from the owner task API (see [tasks.md](tasks.md)). The page shows what the
server reports; it never decides a task's state locally.

- **Project page**: a **Tasks** section lists the project's tasks as a tree, supervisors with their child tasks beneath them. A child whose parent is not loaded is still listed. Each row shows the status and reason, the active provider, any operation in progress, and CI (current CI states require both head SHAs present and equal and a valid observation no older than ten minutes; otherwise unknown).
- **Task page** (`/tasks/<id>`): the task and its parents and children; the active provider and native session; the workspace, environment version and branch; the attempt history, including superseded attempts as read-only history; PR, CI and merge as three separate facts, each shown as unknown when the server has not reported it, a task with no pull request also shows a **Not published yet** chip, and its session ID shows as 8 characters with a copy button, with how long ago they were observed (flagged stale after ten minutes); the step and last confirmed step of any open provider switch or other operation; and the provider's own summary, labelled **Provider summary; unverified**. A report or finished turn is never shown as proof the task is complete. PR links open only when they are `https` URLs, in a new tab with `rel="noopener noreferrer"`.
- Failed task reads, including background updates and reconnects, show a last-known-state warning and retry control on the task page. The warning clears after a successful read of that task; list failures are tracked separately.
- **Actions**: **Retry**, **Reassign** (to the other provider, chosen from the profiles that offer the attempt's role) and **Cancel task**. Each is enabled only when the server's eligibility says so, and sends the task version and attempt shown on the page. If the task changed first, the server refuses with a conflict and the page re-reads the task. If a response is lost, a dedicated **Resend original request** control shows the original target, version and attempt and reuses the same request. It remains available when eligibility or the current provider changes, and is disabled while a request is in flight. The notice says the outcome is unconfirmed. Pending approvals and a required checkpoint are listed as blockers.
- **Updates**: `task:updated` events only prompt a re-read; duplicates are ignored, and after the event stream reconnects the page re-reads every task it holds.
- **Links**: the session and workspace pages link to the task that owns them (noting when the attempt is superseded), and an Inbox approval card links to its task. Approvals are still answered in the Inbox; the task page only points to it.
- **Mobile**: the same sections in a single column. Buttons and the provider picker are at least 44px tall.

## React status (F5)

- **Implemented:** the desktop inbox panel is always mounted, so its **Needs you** tab carries the count and polls while the page is visible. The compact Inbox tab shows the same panel. Inbox buttons, links and the reply field are at least 44 px tall.
- **Implemented:** on the compact tab bar the Inbox tab carries two badges: the attention count (`InboxBadge`) and the owner-task count (`TasksBadge`). The tab's accessible name includes both counts. Both counts keep polling while another compact tab is open, pause while hidden, and share the visible polling cadence. Details: [inbox.md](inbox.md#react-status-f5).
- **Not built:** a header badge on desktop. The design notes do not ask for one; the desktop count sits on the **Needs you** tab.
- **Not proven:** layout at phone width in a browser. Playwright journeys are deferred to F9.
