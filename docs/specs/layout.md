# Layout

Mainloop is responsive across mobile and desktop viewports.

Project pages load repository details before showing project controls. If that request fails,
the page shows the load error and a **Retry loading project** button instead of remaining on
the loading message. Retrying requests the details for the current project.

## Desktop

- On home (`/`) the centre column is the Mainloop chat alone: one scroll (the thread), the composer at the bottom, nothing stacked above or below it. Other pages scroll in the same column.
- The chrome is set in the system's sans font: navigation, the Explorer, page headings, buttons, forms and status rows. The transcript and its composer are monospace, as is anything that is code (ids, branches, commits, paths, schedules, a task's brief). The `$ mainloop` mark is monospace.
- A conversation keeps to a reading width: the transcript and its composer are centred in the column and no wider than about 100 characters (56 rem), however wide the window is. The scroll area and the session row above it stay the full width of the column. On a phone the conversation fills the width.
- The desktop is laid out as an IDE is, with no top header: an **activity bar** down the left edge, the **sidebar** beside it, the page in the centre, and a **status bar** along the bottom.
- **Activity bar** (44 px wide): the `$` mark, which opens the main chat; **Sessions** and **Projects**, which choose the sidebar's view; **Inbox**, which opens the answer view and carries the Needs-you count as a red badge (its accessible name includes the count); and at the foot **Orchestration health**, **Settings** and the theme menu. Choosing the view that is already showing hides the sidebar; choosing either view shows it. Inbox, Orchestration health and Settings are marked while their page is showing.
- **Sidebar**: on the left, 240–480 px wide (default 300), resizable from its right edge (the right arrow widens it), and hidden with its own button, from the activity bar, or with one shortcut: **⌘B** on a Mac and **Ctrl+B** elsewhere, never both on one platform (on a Mac, Ctrl+B in a text field is the system's "back one character" and is left alone). The shortcut works while typing. Its width, whether it is hidden, its view and each node's expanded state are kept per browser in local storage. It shows one view of the tree at a time:
  - **Sessions**: Mainloop, Needs you, Problems, Since you were here, Working and Finished recently.
  - **Projects**: Projects and Schedules.
- **Status bar** (24 px): on the left the connection, **Connected** with a green light, which links to orchestration health; on the right **N need you** (or **Nothing needs you**), which opens the answer view, **N working**, which shows the Sessions view, and **N problems**, which opens the Problems view. While the backend is unreachable the whole bar turns to the offline colour and reads **Offline since HH:MM · reconnecting…**; nothing is added above the page, so nothing moves. The counts are the same groups the tree draws, and stay on screen whichever view shows and when the sidebar is hidden.
- The tree's groups, top to bottom (the phone shows them all in one list; the desktop sidebar shows them by view): **Mainloop** (Idle, Working or Needs you; its children are the live sessions; clicking it opens the main chat), **Needs you** (expanded by default, with a count), **Working**, **Projects** (each project holds its tasks, and each task its child tasks, sessions and workspaces), **Schedules**, **Finished recently** (tasks completed or cancelled in the last seven days; collapsed by default; its count matches its rows), and, on the phone only, **Insights** (one row whose dot shows the backend connection; it opens `/health`; on desktop the connection is in the status bar). **Since you were here** sits right after Needs you, expanded, when anything changed since the last app open; **N tasks need you** opens the answer view at its item (or `/inbox`), and the other rows (for example **1 task failed**) open the most recent task they count. Its count is the number of distinct tasks. A task with an owner action is listed under Needs you only, never also under Working.
- A task row's primary label is the task's own title, whatever group it sits in, never the project it belongs to. The title has the row's first line to itself, so it is the last thing to be cut short. Under it sits one muted line: the project's short name (`widgets` for `acme/widgets`), the task's state, and how long ago it changed, for example `widgets · Running · 3h`. The brief is not shown on the row. A task stored without a title (there is none today, but the row guards against it) falls back to a snippet of its brief as the label instead.
- A child task (its `parent_task_id` points at another loaded task) nests under that parent wherever the parent is shown, instead of also sitting beside it as a flat row in Working, Finished recently or its project's task list; Needs you stays flat regardless, so an owner action is never hidden a level down. A parent task's detail line shows its direct children's progress instead of its own status — for example `3/5 done · 1 running · 1 blocked`, a part left out once it is zero, and `N need you` in place of blocked or running when a child has an owner action open against it. When the parent or its most recently updated child carries a report, that report's line follows the counts (the parent's own report wins; otherwise the newest child's). A task with no child tasks keeps its plain status detail.
- Selecting **Needs you**, or any row in it, opens the answer view (`/inbox`) in the centre column in place of the chat, with the chosen item brought to the top under the view’s sticky header, and marked. It holds **Needs you** (questions, plan reviews and approvals, answered in place) and **Activity**. A task's **Open Inbox** link and old `?tab=inbox` links open it too. A back link returns to the chat (desktop) or the Explorer tab (phone).
- Buttons, fields, links and menu rows are 28 px tall with a mouse or trackpad and 44 px tall on a touch screen. Rules between regions are quieter than the edges of fields.
- A top-level group (Needs you, Working, Projects and the rest) is a heading in small capitals with its count on the right. **Mainloop** and **Insights** are one line with their state beside the label. Every other row is its title, then one muted line beneath it for what the row is or needs (a task's project, state and age; what an owner action needs; a session's or workspace's state; a schedule's timing); a row with nothing to say there is one line. Indent guides mark each level.
- A row has one status dot, in the same language as herdr's agent list: **filled amber** is working (a task that is running, or waiting on CI or another task); **filled red** is **Needs you** (a question, an approval, or a failure that needs a decision; the line under the title says which); **filled teal** is finished and not yet looked at; **hollow** is at rest (idle, asleep, cancelled, or finished and seen). Accent blue is never a status. The session row above a conversation uses the same dot, so "ready" and "asleep" are hollow. Green is the connection light on **Insights** only.
- A completed task is "not yet looked at" until its task page, or the page of its session, has been opened; a task that changes after that is unseen again. **Implemented per browser:** the record is in local storage (`mainloop.seen.v1`), so it does not yet follow the owner between phone and desktop; there is no server-side marker today. A task that finished before a browser first kept the record counts as seen, and with storage blocked nothing shows as unseen.
- The row for the page being shown is filled and marked as the current page (`aria-current="page"`), wherever it appears in the tree, so the tree shows where the owner is. Keyboard focus is separate: it is the ring.
- Each tree row, including Needs-you rows, has an **Ask Mainloop** button, visible on hover or keyboard focus. It opens the main chat with the composer filled, not sent, for example `About task <title> (<short id>): `. The **a** key does the same from a focused row. Right-click (desktop), a long press (phone) or Shift+F10 and the menu key open a small row menu with **Open** and **Ask Mainloop**. It closes on Escape, a press outside, a scroll of the explorer or its row moving; other scrolling, such as the chat following a turn, leaves it open.
- Keyboard access: up and down move, right expands, left collapses, Enter opens. A ring marks the row with keyboard focus only.
- The sidebar's header is its view's name (**Sessions** or **Projects**; **Explorer** on the phone) and, on desktop, the button that hides it; on the phone it also holds orchestration health and the theme selector. The hint **Ask Mainloop to start, stop or change anything.** shows in the empty main chat, above the suggestion chips.
- There is no create button for tasks, agents or workspaces in the activity bar or the sidebar. Work starts from the main chat. The project page has no create control either; `/agents` is reachable by URL only.
- An empty main chat shows suggestion chips inside the empty thread (they go once the thread has messages): **What needs me?**, **Start a task in <first project>:**, **What’s running?** and **Pause a schedule:**. Each fills the composer without sending.

## Mobile

- There is no header. The page starts at the top of the screen, so on the main chat the one row above the thread is the session row (status, pending count, Session info).
- A bottom tab bar has three tabs with plain labels: **Chat**, **Needs you** and **Explorer**. (The third tab's label is not final; it was not to be "Work".)
  - **Chat** opens the main chat.
  - **Needs you** opens the answer view (`/inbox`). It shows the Needs-you count as a red badge, and its accessible name includes the count ("Needs you, 3").
  - **Explorer** shows the tree, full width, over the current page (`?tab=explorer`). Rows are 44 px touch targets with their second line beneath the title, and the Ask Mainloop button is always shown.
- The lit tab follows where the owner is: Chat on the main chat, Needs you on the answer view, and Explorer both while the tree is showing and on any page opened from it (a task, a session, a workspace, a project, a schedule, orchestration health). On such a page, Explorer brings the tree back and keeps the page's route, so the page's row is still marked as current.
- Orchestration health, Settings and the theme selector are in the tree's header, since there is no header above the page. The tree also has the **Insights** row.
- The answer view has a back link to the Explorer tab.
- Tab bar hidden on desktop viewports.
- Old `?tab=` values `sessions` and `projects` open the Explorer tab; `?tab=inbox` opens the answer view.
- The page and the tree are two panes. Each is built the first time it opens and then stays mounted, so an open tree survives a switch. Chat and Needs you are pages, so moving between them changes the route; a draft in the composer is kept by the draft store.
- The tab bar stays clear of the device's safe areas (notch, rounded corners and the home indicator), as does the top of the page.

## Shell

- On the phone the offline banner is one line at the top of the screen. It takes its height from
  the page, so the composer and the tab bar stay on screen at 390 × 844. Long text is cut off with
  an ellipsis. On desktop there is no banner: the status bar says it (see Desktop).
- Each page sets the document title, for example **Health · Mainloop**.
- A route change moves keyboard focus to the main landmark, so focus does not fall back to the
  page body.

## Home

- **Home is the main chat.** The work list, the home strip and the separate Since strip were removed; the explorer (see Desktop) holds the same groups.
- **Since you were here**: counts of tasks that need you, failed, and merged since the last visit. The waiting row opens the answer view; the failed and merged rows link to the most recent task they count. It is an explorer group (see Desktop), on both layouts. It is left out when nothing changed, and on a first visit, which has no baseline.
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
- **Mobile**: the same sections in a single column. Buttons and the provider options are at least 44px tall.

## React status (F5)

- **Superseded by the explorer:** the always-mounted inbox panel and the compact Inbox tab with its two badges. Attention and owner tasks now poll while the explorer is mounted, on both layouts; the Needs you tab on the phone, and the activity bar's Inbox item and the status bar on desktop, carry the Needs-you count; and the answer view at `/inbox` mounts the same inbox panel. Inbox buttons, links and the reply field are at least 44 px tall.
- **Desktop count:** on the activity bar's Inbox item, in the status bar, and on the sidebar's Needs you heading while the Sessions view shows.
- **Checked in a browser (fixtures, not live):** 390×844 and 1440×900 against the MSW dev server. Playwright journeys are deferred to F9.
