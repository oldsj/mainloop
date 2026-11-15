# Session transcripts

Status: **implemented** (#448): storage, the turn normalizer, ingestion, the copy before a delete,
retention, the read and search API, the MCP tools, and the web view
([sessions](sessions.md#the-transcript)). Tested against fixtures and the fake kagent; not measured
against a live kagent.

Mainloop's conversation (`session_messages`) keeps each turn's user message and final reply. A
session's transcript also keeps what the agent did on the way: its text, each tool call with its
arguments, each tool result, and status messages. Mainloop copies a turn from kagent once the
turn's A2A task has finished, so the transcript outlives the kagent Session. kagent's own records
become unreachable once a Session is deleted or idles out.

## What a turn records

kagent records a harness turn as one A2A task. The user's message and status messages are in its
history. The agent's text and each tool call and result are artifacts, ordered by kagent's
timeline position. Mainloop stores one entry per message, text run, tool call or tool result, in
that order:

| Kind          | What it is                                                                                      |
| ------------- | ----------------------------------------------------------------------------------------------- |
| `user`        | A message the user, or Mainloop on their behalf, sent                                           |
| `assistant`   | The agent's text                                                                                |
| `tool_call`   | A tool call: the tool's name and its arguments as JSON                                          |
| `tool_result` | The call's result, and whether it was an error                                                  |
| `status`      | A status message, such as why a turn failed                                                     |
| `ask`         | The question or approval a waiting turn asks for                                                |
| `thinking`    | A reasoning summary, where the runtime reports one (kagent's Claude and Codex harnesses do not) |
| `other`       | A file or other part, as a one-line placeholder                                                 |

Orchestration-health artifacts are not copied: they carry no content.

## Never storing credentials

Every text is redacted before it is written (`internal/redact`, transcript mode). This covers:

- private keys, across lines;
- authorization, cookie and bearer headers;
- credential-named assignments;
- credentials in URLs;
- JWTs and well-known token prefixes;
- long opaque tokens.

The opaque-token rule keeps two kinds of digest: a bare 40-character lowercase hex string (a git
SHA-1), and 64-character hex that follows `sha256:` or `sha256-`. Bare 64-character hex is
redacted, because keys have that shape.

The search vector is generated from the stored text, so a redacted credential is never indexed or
found by a search.

**Withheld results.** A tool call that reads a credential file or prints credentials keeps the call
itself, but its result is withheld entirely and stored as `[withheld: the call reads credentials]`,
because a credential file's contents need not have a recognisable shape. A call counts as one of
these when any of its arguments names one of the following:

- a file under `.ssh`, `.aws`, `.kube`, `.gnupg` or `.docker`;
- a `.env*` file;
- `.netrc`, `.git-credentials`, `.npmrc`, `.pypirc`, `credentials`, `kubeconfig` or `auth.json`;
- an `id_rsa`, `id_ed25519` or `id_ecdsa` key;
- a `.pem`, `.key`, `.p12` or `.pfx` file;
- a configuration file with "secret" or "credential" in its name.

Commands that print credentials are withheld the same way:

- `kubectl get`, `describe` or `edit` of secrets;
- `gh auth token`;
- `op read`;
- `security find-*-password`;
- `vault read`;
- `aws secretsmanager get-secret-value`;
- `gcloud secrets versions access`;
- `git credential`.

A source file with such a word in its name, such as `internal/credentials/store.go`, is redacted
like any other file, not withheld.

Redaction is best effort. A credential in a shape none of these rules recognise is stored as it was
written.

## Size

An entry's text is cut after redaction. Redaction runs on the whole text first, so a cut never
leaves part of a credential that redaction would have caught. The limits are:

- **A tool result:** 16 KiB. Its start and end are kept, with `… N bytes omitted …` between them.
- **Any other entry:** 32 KiB. Its start is kept.

Each entry records whether it was cut or withheld, and its size before either.

**A budget per kagent Session.** One kagent Session stores at most 20 MiB of entry text
(`TRANSCRIPT_SESSION_BUDGET_MIB`). Past it, entries are still recorded, with their kind, tool and
size, but without text. The budget counts every byte ever stored for the Session; retention does
not give it back.

## Ingestion

The API server follows every kagent Session bound to one of the owner's sessions, archived ones
included. A sweep every 10 seconds reads the Sessions that are due:

- **Finished turns only.** A task is copied once it is completed, failed, canceled or rejected. A
  task that is running, or waiting for an answer or approval, is not: an answered task continues
  the same turn.
- **When a Session is due.** When Mainloop sees a turn it sent end, the Session is due at once.
  Otherwise it is read again 15 minutes after its last read, which picks up turns Mainloop did not
  send.
- **A cursor.** Each Session keeps kagent's page token. The cursor moves past a page only once
  every task on it, and on the pages before it, has been copied. A task that waits for input holds
  the cursor; the finished tasks after it are still copied. Already-copied tasks are recognised by
  their receipt, so a page read again costs no task read.
- **Bounded passes.** One pass reads at most 20 Sessions, and copies at most 50 tasks from each.
  A Session with more left is due again at once.
- **No transaction across kagent reads.** kagent is read with no transaction open. Each task is
  written in a short transaction of its own, so a pass that stops half way repeats work and loses
  none.

**Before a delete.** Every path by which Mainloop deletes a kagent Session first copies the
Session's finished turns: deleting a workspace, a stop forced on an attempt, restarting a session,
and a replayed create. This copy is not held to a sweep pass's limits and runs for at most 20
seconds. It never stops the delete. If it fails or runs out of time, the failure is logged, the
delete goes ahead, and the turns it did not copy are lost. A turn that is still running or waiting
when its Session is deleted is not copied.

**Pages past a waiting task.** A scan reads at most 200 pages. When a waiting task holds the
cursor, the pages past that bound are read once the task finishes.

**When kagent no longer has a Session.** kagent answers "not found" both for a deleted Session and
for a missing Agent, so Mainloop first checks the Agent exists. If it does, the Session is no
longer followed:

- `complete` when every turn Mainloop sent into it was copied;
- `unavailable` otherwise, with the number of turns that were lost.

**Failures.** A Session whose reads fail is read again later, waiting longer after each failure in
a row: 30 seconds, doubling, up to an hour. After 24 failures in a row it becomes `unavailable`.
When kagent is unreachable, or refuses Mainloop's credentials, the pass stops without counting a
failure against any Session.

## Copying once

Each copied task has a receipt (`transcript_tasks`). Copying the same task again writes nothing.
Normalization is deterministic, so an entry's position within its task is stable.

## Reading

`GET /sessions/{id}/transcript` returns a page of the session's stored entries:

- **Order and paging.** Entries come in the order they were copied, which is the order their turns
  finished. Each stored entry has an `id`. Pass the last one as `after` to read the next page. A
  page holds up to `limit` entries: 200 by default, at most 500. `has_more` says whether more
  stored entries follow, and `has_earlier` whether some come before.
- **Reading backward.** `latest=true` returns the last entries, and `before=<id>` the entries just
  before that one. Either way the page is still oldest first. `after`, `before` and `latest` exclude
  each other (422).
- **Sources.** `sources` lists the kagent Sessions the transcript is copied from, newest first. Each
  shows whether it is still followed (`active`), finished (`complete`) or `unavailable`, with the
  reason.
- **The live tail.** When the page reaches the last stored entry, `live` holds the turns not yet
  copied: running, waiting for an answer, or finished since the last sweep. They are read from
  kagent's control plane for this response only, without waking the agent. They are redacted and
  capped exactly as a copy would be, and are not stored. They have no `id` and carry
  `live: true`.
- **Tail bounds.** The tail covers the two newest followed kagent Sessions and the three newest
  uncopied turns, read within 5 seconds. A turn that a sweep has fallen far behind on may be
  missing from it until the sweep catches up.
- **When kagent can't be read.** The stored page is returned whole, and `live_error` says the tail
  could not be read. kagent's own error is logged, not returned.
- **Skipping the tail.** Pass `live=false` to read stored entries only.

Only the owner's sessions can be read; any other session is 404.

## Search

`GET /transcripts/search?q=` searches the owner's stored entries, best match first:

- **The query.** `q` takes web-search syntax: quoted phrases, `or`, and `-word`. It is matched
  against the entry text and the tool name, with English stemming.
- **Filters.** `project_id` and `session_id` narrow the search. `limit` is 20 by default, at most 100.
- **What a hit holds.** The entry's id, session (with its title), task, position, kind, tool and
  time, plus a snippet.
- **The snippet.** It is plain text split into parts, each marked as matching or not. It is never
  markup, so tag-like text in an entry cannot be rendered as HTML. The text-search parser drops
  tag-like tokens from the snippet.

Live tails are not searched.

## MCP tools

Two read-only tools give agents the same transcripts. Neither sends anything to any agent, so
reading a child's transcript costs the child no turn. Both are available to every actor that has
`task_get`, and both answer only within that actor's task authority (see
[delegation](delegation.md)).

**`task_transcript(task_id, attempt_id?, after?, limit?, kinds?, entry_id?)`** reads a task's
transcript:

- **Which session.** By default, the session of the task's latest attempt that has one;
  `attempt_id` picks another attempt.
- **Paging.** Entries are paged like the API: 50 by default, at most 200. `next_after` is the
  `after` for the next page.
- **Filtering.** `kinds` keeps only some entry kinds.
- **Previews.** Each entry shows a preview of at most 2 KiB, with `cut` set when it was shortened.
- **Size of a result.** A result holds at most 64 KiB of entry text. Past that it stops early,
  with `has_more` set.
- **The live tail.** It is included as in the API, when the page reaches the end.
- **One entry whole.** `entry_id` returns that single entry in full, if it belongs to one of the
  task's attempts.

**`transcript_search(q, task_id?, limit?)`** runs the same full-text search, limited to the sessions
of the tasks the actor can read. `task_id` narrows it to one of them. Each hit names its task and
entry, so the caller can read around a hit with `task_transcript(task_id, after=entry_id-1)`. In the
text result, matches are marked `[[like this]]`.

## Retention

A copied turn's entries are deleted 90 days after it was copied (`TRANSCRIPT_RETENTION_DAYS`). Its
receipt is kept, so a turn kagent still lists is not copied again.

Deleting a session, including a workspace purge, deletes its transcript.
