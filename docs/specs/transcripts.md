# Session transcripts

Status: **partly implemented** (#448). The storage and the turn normalizer exist. Ingestion, the
read and search API, the MCP tools and the web view are still to come; until they land, nothing
writes these tables.

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

## Copying once

Each copied task has a receipt (`transcript_tasks`). Copying the same task again writes nothing.
Normalization is deterministic, so an entry's position within its task is stable.

## Retention

Deleting a session, including a workspace purge, deletes its transcript.
