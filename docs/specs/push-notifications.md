# Push notifications

Mainloop can push notifications to the owner's phone so the owner can step away and still hear when
something needs them. There are two channels, each on or off by itself:

- an [ntfy](https://ntfy.sh) server (`server/internal/notify`);
- Apple push (APNs) to each registered iPhone ([devices](devices.md)) running the Mainloop app
  (`server/internal/apns`).

Both send the same notifications (below); with both on, the owner gets each one twice, once per
channel.

## Configuration

ntfy is off unless both of these are set on the server:

| Variable              | Meaning                                                     |
| --------------------- | ----------------------------------------------------------- |
| `MAINLOOP_NTFY_URL`   | ntfy base URL, such as `http://ntfy.ntfy.svc`               |
| `MAINLOOP_NTFY_TOPIC` | topic to publish to (letters, digits, `-`, `_`; at most 64) |
| `MAINLOOP_NTFY_TOKEN` | optional access token, from a Secret; never committed       |

The token needs **read and write** access to the topic: a retry reads the topic back before
posting again (see below). Setting only one of URL and topic is a startup error. Links use
`FRONTEND_SCHEME://FRONTEND_DOMAIN`. For APNs, see [Apple push](#apple-push-apns).

## What is sent

| Event                                        | Title      | Body            | Opens           | Priority |
| -------------------------------------------- | ---------- | --------------- | --------------- | -------- |
| New Needs-you approval                       | item title | what is needed  | the item's card | 4 (high) |
| New Needs-you question or owner decision     | item title | what is needed  | the item's card | 3        |
| Root task completed                          | task title | Task completed  | `/tasks/<id>`   | 3        |
| Root task failed                             | task title | Task failed     | `/tasks/<id>`   | 4        |
| Root task blocked (not on an owner decision) | task title | Task blocked: … | `/tasks/<id>`   | 4        |

- Only Needs-you items that are an approval, question or owner decision count. Problems (kind `problem`), low-priority reviews (kind `review`, such as project memory proposals) and Activity are not pushed.
- A task blocked on an owner decision is pushed once, as its attention item, not twice.
- Child tasks are not pushed; their root reports.
- Items and task changes from before push was first turned on (either channel) are not sent (a
  floor is recorded the first time the sweep runs), so enabling push does not page for everything
  already open.
- Main finishing a turn is **not** pushed. It would fire on every reply, and Mainloop has no
  signal for whether the owner is away, so it would mostly be noise. Not built.

## Delivery

While either channel is on, every five seconds a sweep writes one `push_notifications` row per new item or task change, keyed
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

With ntfy off, its rows are recorded `skipped` instead of sent; they still feed APNs.

## Apple push (APNs)

APNs is on when the API finds its credentials: an APNs token-auth key (the `.p8` file), its key ID
and the team ID, as files `key.p8`, `key-id` and `team-id` in a directory.

| Variable                        | Meaning                                          |
| ------------------------------- | ------------------------------------------------ |
| `MAINLOOP_APNS_CREDENTIALS_DIR` | the directory, mounted from a Secret             |
| `MAINLOOP_APNS_TOPIC`           | the app's bundle ID; required with the directory |

With none of the three files there (the Secret does not exist yet), APNs is off and the API logs
it. With some missing, or a key ID, team ID or key that is malformed, the API does not start. The
API reads the files at startup only. The chart mounts the Secret optionally, into the API container
only (`apns.*` values, see the chart's README). One key serves every app of an Apple team; the
topic names the app.

### Device tokens

The iPhone app asks for permission to notify after it registers ([iOS app](ios-app.md)), then sends
the token Apple gave it, signed as its device:

- `PUT /devices/current/push` with `{"token": "<lower-case hex>", "environment": "development" |
"production"}`. The environment is the one the app was built for (its `aps-environment`
  entitlement), which picks Apple's host (`api.sandbox.push.apple.com` or `api.push.apple.com`).
  The app sends it on every launch. Setting a token after a refusal (below) clears the refusal and
  counts as a new token: the phone gets what opens from then on, not what opened while push to it
  was stopped. A token belongs to one device: setting it moves it from any other.
- `DELETE /devices/current/push` stops push to the device.
- Revoking a device clears its token. The app's sign-out is a revoke, and the app also unregisters
  from APNs, so a token the revoke could not clear (the phone was offline) is refused by Apple
  from then on.
- Only a registered device sets or clears its own token: the owner's other credentials get `403`.

### What a phone gets

Each notification fans out to one `apns_deliveries` row per active device with a valid token,
keyed by notification and device, so sweeps, replicas and restarts never write a second row. A
device gets only notifications written after it first set a token: registering a phone does not
page for what is already open. A revoked device gets nothing more.

The payload carries the title and what is needed, never an item's content: no command arguments,
diffs, secrets or tokens. Titles and bodies are folded to one line, redacted of credential shapes
and shortened (120 and 300 characters).

```json
{
  "aps": {
    "alert": { "title": "<item or task title>", "body": "<what is needed>" },
    "sound": "default",
    "badge": 3,
    "thread-id": "needs-you",
    "category": "NEEDS_YOU",
    "interruption-level": "time-sensitive"
  },
  "mainloop": { "type": "attention", "item_id": "queue:q1", "href": "/inbox" }
}
```

- `badge` is the number of items in Needs you when it is sent (`AttentionSnapshot.count`), the
  number the app shows. It is left out if that count cannot be read.
- An attention item has `thread-id` `needs-you`, category `NEEDS_YOU` and its Needs-you id as
  `item_id` (with `task_id` when it has one). A task report has `thread-id` `task:<id>`, category
  `TASK` and `task_id`.
- Priority 4 (approvals, failures, blocks) is `time-sensitive`; the rest `active`.
- Hiding the text on a locked phone is iOS's own **Show Previews** setting; the app's categories
  give a placeholder instead.

### Sending

A send loop claims each delivery (reconciler leases, no transaction open across the request) and:

1. Records it `suppressed` without sending if the device was revoked or has no valid token, the
   notification is older than 12 hours, or the owner no longer needs it (as for ntfy).
2. Posts it over HTTP/2 to `/3/device/<token>` with a provider token (ES256, `kid` the key ID, `iss`
   the team ID), replaced every 40 minutes. APNs has no idempotency key and nothing to read back,
   so an uncertain attempt is sent again. Every attempt carries the same `apns-id` (the delivery's)
   and `apns-collapse-id` (the notification's), so a second copy replaces the first on the phone.
   `apns-expiration` is 12 hours after the notification.
3. Reads the answer:
   - accepted: `sent`;
   - `410`, or `400` `BadDeviceToken` or `DeviceTokenNotForTopic`: the token is marked refused, the
     delivery `failed`, and nothing more goes to it until the phone sets a token again;
   - `403` `ExpiredProviderToken`: a new provider token (at most every 20 minutes), then retry;
   - `429`, `5xx`, a wrong key or team (`InvalidProviderToken`), or no answer: retry with backoff,
     up to eight attempts, then `failed`;
   - any other `4xx`: `failed`.

Not proven: delivery to a real phone. Tests run against a fake HTTP/2 APNs server and a fake
sender.
