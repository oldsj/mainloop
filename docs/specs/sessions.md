# Sessions

Sessions are background AI work spawned from the main thread. Each session has its own conversation and runs independently.

## Session List

Desktop shows sessions in a sidebar. Mobile shows sessions in a tab.

When no sessions exist:

- Shows empty state with "No sessions yet" message
- Shows hint: "Sessions appear when Claude spawns background work"

When sessions exist:

- Each session shows title and status badge
- Active count shown in header (e.g., "2 active")
- Clicking a session opens its detail view

## Status Badges

| Status          | Badge       | Meaning                  |
| --------------- | ----------- | ------------------------ |
| pending         | PENDING     | Queued, not started      |
| active          | ACTIVE      | Currently running        |
| waiting_on_user | NEEDS INPUT | Blocked on user response |
| completed       | DONE        | Finished successfully    |
| failed          | FAILED      | Error occurred           |

Failed sessions show error message below the badge.

## Session Detail View

Clicking a session navigates to `/sessions/{id}`:

- Shows title as h1 heading
- Shows description if present
- Has Chat and Logs tabs (Chat tab active by default)
- Active sessions show Cancel button
- Completed sessions show Summary section
- Failed sessions show Error section
- Back button returns to home
- Non-existent session ID shows "Session not found" with link to home

## Notifications

When a session needs attention:

- Toast notification appears with title and preview
- Clicking notification navigates to that session's detail view
