# Git publication authority

## Go implementation and evidence

The Go gate serves synthetic receive-pack advertisements and validated single-branch pushes on
`serve-git` port 8004. It requires `GIT_TRANSPORT_ENABLED`, `PUSH_GATE_ENABLED` and a configured
GitHub App. Disabled requests refuse before authentication or upstream traffic. Port 8003 keeps
the read transport. `/health` and `/ready` remain listener probes.

Scratch PostgreSQL, fake Kubernetes Secrets, a fake GitHub App API and local bare repositories
prove branch creation, fast-forward, real smart-HTTP `git push`, replay, refusals and uncertainty
resolution. Workspace creation tests additionally prove creation-time attachment, replay,
readiness and release; a credential read from a fake workspace Secret publishes through the real
gate to a local bare repository. No live GitHub, Actor containment or production rollout is claimed.

Native workspace creation freezes the lowercase `authorization` reference from
`pushgate.Reference` before `CreateSession`. Only a current code-task attempt with the exact held
writer generation, repository and feature branch reserves one; standalone and default-branch
workspaces freeze no push reference. The durable `workspace_push_reservations` intent uses the
attempt as its idempotency key and the attempt's native creation request ID as its identity.
Retries and deletion replays carry the saved reference, even after release or configuration changes.
The native idempotent create checks the original request before returning or creating its Session.

The workspace enrollment reconciler freshly reads the native Session on every retry. It requires
READY, no operation and exactly one matching creation-time reference before calling
`Service.Enroll`, which freezes the active task/writer authority and runtime and publishes the
immutable Secret. No reference is added to an existing Session. A refused, abandoned, failed,
deleted or expired creation releases the reservation; release also revokes any push grant and
credential, with Secret deletion handled by the existing credential cleanup loop. The reservation
expires after the configured workspace create retry window if enrollment never completes.
A delete request revokes the reservation, grant and push credential in the transaction that records
the delete intent, and the gate refuses any workspace that has a delete op, in any state, so a
delayed or failing native delete cannot leave a window for a new push. A push already dispatched is
not recalled. A delete later rejected (for example because a turn is open) leaves the workspace
without push authority on purpose: it fails closed and the terminal reservation cannot reopen.
The release sweep also settles a grant or push credential still planned or published behind an
already revoked reservation (a crash between `Enroll` and its fence), so the Secret is cleaned up.
Terminal reservations and their identities cannot reopen or change. No owner policy API is added,
and authentication never issues credentials.

## Authority and policy

A push bearer is a published `git_push` credential bound to a published grant and enrolled
repository. Read, MCP and another workspace's credentials supply no push authority. The stored
workspace, session, binding and project must agree on owner and repository; ended, archived,
superseded or deleted bindings refuse. Repository names compare without case; branches compare
with case.

Every grant freezes the branch, owner, native Session, writer generation, optional task attempt,
policy version and a digest of the confirmed native runtime. Fresh GetSession must match the
stored checkout, Agent and accepted development environment and show READY with no operation,
a prepared revision, composition and current active generation/atespace/Actor name/UID. Runtime
replacement refuses the old grant. A copied Session ID or client header supplies no authority.

Owner workspace writers require a held binding claim. Delegated writers additionally require the
active current G8b task attempt, the same binding/session/workspace, the exact held branch claim
and writer generation, the correct role and an open active parent when present. Authority is
checked before processing and again after seed reads and remote-ref discovery, before committing
dispatch. Short transactions serialize admission against writer release and credential/policy
changes; no database transaction or row lock spans native, Secret or GitHub I/O.

Before reading a receive body, the gate commits a leased admission claim for its repository and
branch, then reserves `min(body limit, disk budget)` bytes from a process-wide scratch budget.
Separate pods share the branch claim; each pod has its own disk budget. Another live admission
for that branch or insufficient unreserved scratch returns HTTP 503 with the closed
`admission_busy` code and `Retry-After: 1`, before reading the body or creating any spool file.
Admission also blocks writer release/transfer. Claims and reservations remain held through
scratch cleanup and are released on every outcome. A crashed handler's claim expires after the
bounded request/dispatch/persistence window; stale handlers cannot reserve or dispatch a
publication, and their cleanup cannot release a successor's claim.

Before validation, the body reservation must expand to the full per-quarantine disk allowance.
That allowance covers the body, seed pack, repository files and temporary Git output in the same
process-wide budget. Validation is consequently serialized at the pod's disk budget; other
branches can spool concurrently before validation, but insufficient validation capacity returns
the same retryable `admission_busy` refusal. Capacity stays reserved through dispatch and cleanup.
The eight-branch regression checks real quarantine sizes against this reservation, refuses the
seven competing branches while validation is held, and proves they succeed after cleanup.

Policy requires a known default branch. That branch, every previously observed default and
protected patterns override the allowed patterns. Patterns match case sensitively, with `*`
covering nested branch names as in Python's `fnmatchcase`; malformed patterns refuse. An empty
allowlist permits other valid feature branches. `SetPolicy` is an internal compare-and-swap:
versions increase by one and former defaults cannot be removed. Existing grants remain subject
to the current policy. A remote default disagreement refuses publication until trusted metadata
and policy agree.

The `project-metadata` sweep (see [projects](projects.md#metadata)) is what makes them agree. It
fills an empty `projects.default_branch`, which unblocks enrollment (`metadata_unavailable`
otherwise). When GitHub's default branch changes, it calls `AdvanceDefaultBranch`, which reads the
policy and moves the new default in through `SetPolicy`'s compare-and-swap, keeping the owner's
patterns and retiring the old default into the previous defaults, so pushes to either are refused.
A project with no policy yet is left for enrollment, and a policy already naming the branch is not
changed. A lost swap or a GitHub error leaves everything as it was and is retried by the next
pass. The policy advances before the project row, so a failure between the two is refused as a
disagreement until the next pass completes it.

Exactly one `refs/heads/<granted-branch>` update is allowed, creating a branch or fast-forwarding
its trusted current head. Deletion, tags, notes, multiple refs, rewind/divergence, protected or
default branches, missing objects and missing authority refuse. LFS routes and push certificates
are unsupported.

## Protocol and quarantine

The Host is the configured push Service host (loopback is also permitted in development).
Forwarding/rewrite/proxy/actor/session headers, duplicate headers, control characters, gzip,
other HTTP encodings, contradictory framing and non-v0 Git protocols refuse. Neither client
headers nor paths are forwarded upstream. Responses return a server-generated 32-digit hex
correlation ID in `x-mainloop-request-id`. Refusal logs contain only that ID and closed code/stage
fields, plus standard log metadata. Unknown classifications become `transport_unavailable` or
`unknown`; request fields, packet content, credentials and exception text are omitted.

Git's exact complete four-byte `0000` receive-pack probe returns HTTP 200 with an empty
`application/x-git-receive-pack-result`, after route, framing, authentication, binding, policy,
runtime, writer-generation and unresolved-publication checks. It creates no publication, seeds no
objects, mints no App token and sends no upstream traffic. It holds normal admission while
checking at most five bytes in memory and creates no spool file. Empty, truncated, extra-byte and
shallow-only bodies remain invalid. The subsequent real POST passes independent validation.

Receive bodies spool privately on disk under `/var/lib/mainloop/git`, with mode 0700 and current
process ownership; tmpfs/ramfs and symlink roots refuse. Spool files are removed on every outcome.
Defaults are 256 MiB body, 256 KiB commands, 100,000 objects, 512 MiB reconstructed objects, 2 GiB
scratch disk and 64 MiB free reserve. Spooling, validation and dispatch each have a 60-second bound.
The configured body and command limits are used by the listener. The fixed configuration disk
budget is sized below the 3 GiB spool volume; configuration rejects a body limit above 2 GiB.
Each copy stops at `min(body limit, disk budget) + 1` bytes, and checks remaining free space
before each body/seed allocation to preserve the 64 MiB reserve. Seed limits include existing
repository files. Before every Git subprocess, available space must cover its entire remaining
reserved disk allowance plus the 64 MiB headroom. A non-commit branch head
returns HTTP 403 with `commit_required`.

Strict pkt-lines require lowercase SHA-1 OIDs, one command and supported report-status
capabilities. Distinct nonzero `shallow <oid>` lines are accepted only before commands and are
forwarded unchanged. They never create trusted ancestry: validation seeds full history for the
target and default branches through the repository-scoped read port.

Before seeding, bounded zlib parsing checks PACK framing, object/expanded-size bounds, SHA-1
checksum and trailing bytes. Git then runs in a private bare quarantine with `index-pack --strict`
(and `--fix-thin` for incoming objects), `fsck --strict`, commit-type and ancestry checks. A measured
object inventory checks reconstructed count and size, including thin bases. `prlimit` caps each
Git subprocess at 1 GiB address space, 30 CPU seconds, 64 descriptors and the remaining file
budget. Hooks, protocols, inherited configuration, replacements and credential helpers are
disabled. Outputs and filesystem use are capped. Shallow grafts cannot excuse missing history.

GitHub read tokens are scoped to one repository with `contents:read`; metadata and publication
use independently scoped `metadata:read` and `contents:write` App tokens behind the shared GitHub
endpoint allowlist. Redirects are refused and replies are bounded. Tokens remain inside the
adapter. Raw receive replies/progress are never relayed or stored: only a locally synthesized
report and bounded receipt classification reach the client or ledger.

## Publication ledger and uncertainty

A validated command and immutable body/authority identity reserve a `pending` intent before
seed/CPU work while the earlier admission claim still holds the branch. The publication fence
retains unresolved durable outcomes after transient admission is released.
The workspace and server-generated request ID supply the operation's idempotency key; the ID is
also retained for correlation with refusal logs. A confirmed identical body and credential return
the recorded result without dispatching again. An explicit retry after rejection creates a new
intent and independently checks the current remote head before any publication. Pending, dispatching and unknown
publications block another push or probe, and a database trigger blocks writer release/transfer.
After an active handler has released admission, an unresolved publication continues to refuse
with `publication_unresolved` until the resolver establishes its outcome.
The audit ledger survives workspace/session deletion.

After measured validation and fresh authority/ref checks, the handler commits `dispatching`
before sending upstream bytes. An unpack report and exactly one matching ref report classify
confirmation or rejection. HTTP 200 alone proves neither. Cancellation after admission does not
cancel bounded dispatch/outcome persistence. Lost, malformed, reflected or incomplete replies,
timeouts and uncertain persistence retain `unknown` or an unresolved committed row. Immutable
identity cannot change and terminal states cannot reopen.

The `serve-api` reconciler claims expired pending/dispatching rows and unknown rows. It observes
the remote ref without a transaction and never resends the body. Abandoned pending intents are
rejected as not dispatched. For uncertain dispatches, the exact new head confirms remote
convergence; the unchanged old head rejects it. A third head, unavailable remote or incomplete
historical evidence retains the fence with backoff. Resolver evidence is distinguished from a
receive-pack receipt: remote equality proves convergence, not which request caused it.

External operator changes and runtime failure can occur after the last observation and cannot
recall an already admitted push. Git's old-head compare-and-swap rejects a changed remote head.
Native credential attachment, Actor egress containment, packaging, GitOps rollout and live use
remain release gates. The frozen Python implementation is the behavioral reference; its lack of
an unknown resolver and connection locks through dispatch are deliberately superseded here.
