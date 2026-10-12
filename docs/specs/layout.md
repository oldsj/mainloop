# Layout

Mainloop is responsive across mobile and desktop viewports.

Project pages load repository details before showing project controls. If that request fails,
the page shows the load error and a **Retry loading project** button instead of remaining on
the loading message. Retrying requests the details for the current project.

## Desktop

- On home (`/`) the centre column is the Mainloop chat alone: one scroll (the thread), the composer at the bottom, nothing stacked above or below it. Other pages scroll in the same column.
- Explorer panel on the right, styled like an IDE sidebar. It is resizable (240–480 px, default 384) and collapses to a thin rail with the Needs-you count. Width, collapsed state and each node’s expanded state are kept per browser in local storage.
- Explorer tree, top to bottom: **Mainloop** (idle, working or waiting on you; its children are the live sessions; clicking it opens the main chat), **Needs you** (expanded by default, with a count), **Working**, **Projects** (each project holds its tasks, and each task its sessions and workspaces), **Schedules**, **Finished recently** (tasks completed or cancelled in the last seven days; collapsed by default; its count matches its rows), and **Insights** (one row whose dot shows the backend connection; it opens `/health`). **Since you were here** sits right after Needs you, expanded, when anything changed since the last app open; **N tasks waiting on you** opens the answer view at its item (or `/inbox`), and the other rows (for example **1 task failed**) open the most recent task they count. Its count is the number of distinct tasks. A task with an owner action is listed under Needs you only, never also under Working.
- A task row's primary label is the task's own title, whatever group it sits in, never the project it belongs to. Under it sits a short snippet of the task's brief (its first ~140 characters, flattened to one line), so the owner can tell sessions apart without opening each one, and a small bordered tag naming the task's project. A task stored without a title (there is none today, but the row guards against it) falls back to a snippet of its brief as the label instead, and leaves out both the repeated snippet and the project tag.
- Selecting **Needs you**, or any row in it, opens the answer view (`/inbox`) in the centre column in place of the chat, with the chosen item brought to the top under the view’s sticky header, and marked. It holds **Needs you** (questions, plan reviews and approvals, answered in place) and **Activity**. A task's **Open Inbox** link and old `?tab=inbox` links open it too. A back link returns to the chat (desktop) or the Explorer tab (phone).
- Rows are one dense line on desktop (label, status, count) with indent guides per level; a task row adds its brief snippet as a second, muted line beneath that.
- Each tree row, including Needs-you rows, has an **Ask Mainloop** button, visible on hover or keyboard focus. It opens the main chat with the composer filled, not sent, for example `About task <title> (<short id>): `. The **a** key does the same from a focused row. Right-click (desktop), a long press (phone) or Shift+F10 and the menu key open a small row menu with **Open** and **Ask Mainloop**. It closes on Escape, a press outside, a scroll of the explorer or its row moving; other scrolling, such as the chat following a turn, leaves it open.
- Keyboard access: up and down move, right expands, left collapses, Enter opens. A ring marks the row with keyboard focus only.
- The explorer header carries the hint **Ask Mainloop to start, stop or change anything.**
- There is no create button for tasks, agents or workspaces in the header or the explorer. Work starts from the main chat. The project page has no create control either; `/agents` is reachable by URL only.
- The header has a **health** link to orchestration health and the theme selector.
- An empty main chat shows suggestion chips inside the empty thread (they go once the thread has messages): **What needs me?**, **Start a task in <first project>:**, **What’s running?** and **Pause a schedule:**. Each fills the composer without sending.

## Mobile

- The header has an icon link to orchestration health, labelled "Orchestration health", and the theme selector.
- Bottom tab bar with two tabs, **Chat** and **Explorer**. The Explorer tab shows the Needs-you count, and its accessible name includes it. The explorer shows the same tree full width. Rows are 44 px touch targets with the status on a second line, and the Ask Mainloop button is always shown.
- The answer view opens over the Chat tab, with a back link to the Explorer tab.
- Chat tab active by default on load
- Tab bar hidden on desktop viewports
- Touch targets sized appropriately for mobile interaction
- Old `?tab=` values `sessions` and `projects` open the Explorer tab; `?tab=inbox` opens the answer view.
- Switching tabs keeps what each tab holds, such as a draft or an open tree. A tab is built the first time it opens.
- The header and tab bar stay clear of the device’s safe areas (notch, rounded corners and the home indicator).

## Shell

- The offline banner is one line at the top of the screen. It takes its height from the page, so
  the composer and the tab bar stay on screen at 390 × 844. Long text is cut off with an ellipsis.
- Each page sets the document title, for example **Health · Mainloop**.
- A route change moves keyboard focus to the main landmark, so focus does not fall back to the
  page body.

## Home

- **Home is the main chat.** The work list, the home strip and the separate Since strip were removed; the explorer (see Desktop) holds the same groups.
- **Since you were here**: counts of tasks waiting on you, failed, and merged since the last visit. The waiting row opens the answer view; the failed and merged rows link to the most recent task they count. It is an explorer group (see Desktop), on both layouts. It is left out when nothing changed, and on a first visit, which has no baseline.
- **Last visit**: kept per browser in local storage. The app shell reads it once when it opens and records the current time, so the desktop explorer and the phone tabs share one baseline. Because the desktop explorer shows on every route, the visit is the app's last open, not only the last time home was shown. This is deliberate: the baseline is the last app open, not the last home view. If storage is blocked, the Since you were here group has no baseline and is left out.
- **Updates**: the list refreshes from the same task invalidation as the task pages. The relative times refresh each minute on the page.

## Tasks

Durable tasks are read from the owner task API (see [tasks.md](tasks.md)). The page shows what the
server reports; it never decides a task's state locally.

- **Project page**: a **Tasks** section lists the project's tasks as a tree, supervisors with their child tasks beneath them. A child whose parent is not loaded is still listed. Each row shows the status and reason, the active provider, any operation in progress, and CI (current CI states require both head SHAs present and equal and a valid observation no older than ten minutes; otherwise unknown).
- **Task page** (`/tasks/<id>`): the task and its parents and children; the active provider and native session; the workspace, environment version and branch; the attempt history, including superseded attempts as read-only history; PR, CI and merge as three separate facts, each shown as unknown when the server has not reported it, a task with no pull request also shows a **Not published yet** chip, meaning no PR yet, and its session ID shows as 8 characters with a copy button, with how long ago they were observed (flagged stale after ten minutes); the step and last confirmed step of any open provider switch or other operation; and the provider's own summary, labelled **Provider summary; unverified**. A report or finished turn is never shown as proof the task is complete. PR links open only when they are `https` URLs, in a new tab with `rel="noopener noreferrer"`.
- Failed task reads, including background updates and reconnects, show a last-known-state warning and retry control on the task page. The warning clears after a successful read of that task; list failures are tracked separately.
- **Actions**: **Retry**, **Reassign** (to the other provider, chosen from the profiles that offer the attempt's role) and **Cancel task**. Each is enabled only when the server's eligibility says so, and sends the task version and attempt shown on the page. If the task changed first, the server refuses with a conflict and the page re-reads the task. If a response is lost, a dedicated **Resend original request** control shows the original target, version and attempt and reuses the same request. It remains available when eligibility or the current provider changes, and is disabled while a request is in flight. The notice says the outcome is unconfirmed. Pending approvals and a required checkpoint are listed as blockers.
- **Updates**: `task:updated` events only prompt a re-read; duplicates are ignored, and after the event stream reconnects the page re-reads every task it holds.
- **Links**: the session and workspace pages link to the task that owns them (noting when the attempt is superseded), and an Inbox approval card links to its task. Approvals are still answered in the Inbox; the task page only points to it.
- **Mobile**: the same sections in a single column. Buttons and the provider picker are at least 44px tall.

## React status (F5)

- **Superseded by the explorer:** the always-mounted inbox panel and the compact Inbox tab with its two badges. Attention and owner tasks now poll while the explorer is mounted, on both layouts; the Explorer tab and the desktop rail carry the Needs-you count; and the answer view at `/inbox` mounts the same inbox panel. Inbox buttons, links and the reply field are at least 44 px tall.
- **Not built:** a header badge on desktop. The design notes do not ask for one; the desktop count sits on the explorer's Needs you row and the collapsed rail.
- **Checked in a browser (fixtures, not live):** 390×844 and 1440×900 against the MSW dev server. Playwright journeys are deferred to F9.
