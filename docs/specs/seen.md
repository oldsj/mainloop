# Seen state

The owner's seen state: which tasks they have looked at, and when they last opened the app. It is
kept per owner on the server, so it follows them between devices: a task opened on the phone is
seen on the desktop too.

**Implemented (server):** the API below. **Implemented (web, checked against MSW, not a live
backend):** the explorer's unseen dots read `GET /seen` and mark with `PUT /seen/tasks/{task_id}`,
and "Since you were here" takes its baseline from `POST /seen/visits`; see [layout](layout.md) for
when each is called. The web app no longer keeps either record in local storage, and removes the
old keys (`mainloop.seen.v1`, `mainloop.home.lastVisit`) when it opens.

## API

- `GET /seen` returns `since`, `last_visit`, `previous_visit` and `tasks` (`task_id`, `seen_at`).
  With no record yet, `since` is now, both visits are `null` and `tasks` is empty, so nothing is
  unseen. Nothing is written by a read.
- `POST /seen/visits` records that the owner opened the app now and answers with the state after
  it: `last_visit` is now and `previous_visit` the visit before it, from whichever device made it.
  `previous_visit` is the baseline for **Since you were here**; it is `null` on the first visit.
- `PUT /seen/tasks/{task_id}` records that the owner has the task, or the page of one of its
  sessions, open now. Only the owner's own tasks (`404` otherwise). The time is the database's
  clock, the same clock that stamps the task's `updated_at`, so a mark is never earlier than the
  change it acknowledges, whatever the device's clock says. A later mark wins: an earlier one never
  replaces it.

The first visit or mark starts the record and sets `since`; it does not move after that.

## Unseen

A task is unseen when it last changed (`updated_at`) after `since` and after its `seen_at`, or it
has no `seen_at`. A task that changed before the record started counts as seen, so starting the
record does not light every old task.

## Retention

A mark is kept for 60 days, as the local-storage record kept it: an older one is not listed, and
it is deleted on the owner's next mark. The task is then judged as if never opened. A deleted
task's mark is deleted with it. The visits are one row per owner.
