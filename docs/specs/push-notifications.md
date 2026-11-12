# Push notifications

Mainloop can push notifications to the owner's phone through an [ntfy](https://ntfy.sh) server,
so the owner can step away and still hear when something needs them. This is implemented in
`server/internal/notify`.

## Configuration

Push is off unless both of these are set on the server:

| Variable | Meaning |
| --- | --- |
| `MAINLOOP_NTFY_URL` | ntfy base URL, such as `http://ntfy.ntfy.svc` |
| `MAINLOOP_NTFY_TOPIC` | topic to publish to (letters, digits, `-`, `_`; at most 64) |
| `MAINLOOP_NTFY_TOKEN` | optional access token, from a Secret; never committed |

The token needs **read and write** access to the topic: a retry reads the topic back before
posting again (see below). Setting only one of URL and topic is a startup error. Links use
`FRONTEND_SCHEME://FRONTEND_DOMAIN`.

## What is sent

| Event | Title | Body | Opens | Priority |
| --- | --- | --- | --- | --- |
| New Needs-you approval | item title | what is needed | the item's card | 4 (high) |
| New Needs-you question or owner decision | item title | what is needed | the item's card | 3 |
| Root task completed | task title | Task completed | `/tasks/<id>` | 3 |
| Root task failed | task title | Task failed | `/tasks/<id>` | 4 |
| Root task blocked (not on an owner decision) | task title | Task blocked: … | `/tasks/<id>` | 4 |

- Only Needs-you items count. Problems (kind `problem`) and Activity are not pushed.
- A task blocked on an owner decision is pushed once, as its attention item, not twice.
- Child tasks are not pushed; their root reports.
- Items and task changes from before push was first turned on are not sent (a floor is recorded
  the first time the sweep runs), so enabling push does not page for everything already open.
- Main finishing a turn is **not** pushed. It would fire on every reply, and Mainloop has no
  signal for whether the owner is away, so it would mostly be noise. Not built.

## Delivery

Every five seconds a sweep writes one `push_notifications` row per new item or task change, keyed
by what it is about (`attention:<item>`, `task:<task>:<status>:<n>`). A key that exists is
skipped, so sweeps, replicas and restarts never send a second copy: one notification per item, not
per poll. A task gets a new notification only when its status differs from the one last pushed
for it; a task that blocks, resumes and blocks again is not pushed a second time.

A send loop claims each row (reconciler leases, no transaction open across the request) and:

1. On a retry, reads the topic back (`/<topic>/json?poll=1`) and records the row `sent` if an
   earlier attempt already arrived. ntfy has no idempotency key, so this check replaces one. This
   relies on ntfy's message cache (12 hours by default).
2. Rereads whether the owner still needs it: the attention item is still visible in Needs you, or
   the task still has the reported status. If not, the row is `suppressed` and nothing is sent.
3. Posts it. A 4xx other than 429 is `failed` at once; other errors retry with backoff, up to
   eight attempts, then `failed`.
