# Git publication authority

## Implemented source, default off

`GIT_TRANSPORT_ENABLED` and `PUSH_GATE_ENABLED` default to `false`. Source includes injectable
read/push ASGI applications, PostgreSQL authority and native enrollment callers. This slice
installs no production listener, image, route, credential mount or Actor containment. Real local
PostgreSQL with fake kagent/Kubernetes and a fixed local Git upstream proves source behavior only.

Trusted outbound read and push clients use the same owner's existing PAT outside the actor.
Actor capabilities are independent for the exact origins
`http://mainloop-git-read.mainloop.svc.cluster.local` and
`http://mainloop-git-push.mainloop.svc.cluster.local`. Neither purpose grants MCP or owner API
access. No new GitHub credential or principal is introduced. Legacy random hash-only grants
remain compatible when Git transport is disabled; they publish no Git Secret and cannot
authenticate on the new listener. Storing a Session UUID no longer enrolls a grant.

## Frozen enrollment and publication

After workspace/attempt admission commits, Mainloop freezes the original CreateSession identity,
owner/project/repository/branch, Agent, checkout, accepted development environment,
MCP/read/optional-push references, attempt/claim generation and reserved push version. Default
and protected workspaces reserve read only; coordination sessions receive no Git plan. Historical
or dispatched bindings without a plan cannot be retrofitted. Missing dispatch history is a hold.

Issuance uses HMAC-SHA256 with the existing `AGENT_TOKEN_KEY`, a versioned Git domain, independent
purpose and immutable issuance/version/binding/create identity. Read values start with `gread_`;
push values retain `push_`. PostgreSQL stores hashes and references, never capability or PAT bytes.
Missing keys and recovery hash mismatches fail closed, without rotating the issuance.

The complete tuple and dispatch marker commit before CreateSession bytes. Usable Git Secrets
remain absent. Mainloop runs a durable, owned non-turn Suspend/Resume warmup on the same Session,
then obtains a fresh GetSession. READY, Create, List and Resume replies supply no Git association.
Confirmation requires the exact stored Session/context/Agent/checkout/environment, prepared
revision, reported composition and current active field-20 generation/atespace/Actor name/UID.
The confirmed association is immutable. A changed generation or UID refuses old capabilities.

Read publication follows confirmation; delegated creating targets receive read only. Push follows
active/current admission. Native send confirms required publications before external turn bytes.
Unknown create recovery, including revoked cancellation recovery, uses the complete original
tuple without restoring hashes or republishing. Replacement requires a new create identity.

Git Secrets are immutable Opaque objects named `mainloop-git-read-<issuance-id>` and optional
`mainloop-git-push-<issuance-id>`, with one `authorization` key containing the complete Bearer
value, and actor-egress/purpose/binding/issuance labels. Publication creates or compares the exact
existing object, never overwrites conflicts, and records its UID. Lost replies regenerate the
same tuple/value/version. A read-only tuple cannot later gain a push reference.

## Current dispatch authority

Every request resolves an immutable server-side credential stamp. Read proof requires owned
project/session/workspace repository agreement, a live binding and its exact enrolled runtime.
Default/protected reads need no PushGrant. Delegated reads also require current creating/active
attempt, exact role/depth/tree, live parent ancestry and held claim generation. Creating read
preparation grants no MCP, preview, resume or turn admission.

Push additionally requires a live grant and policy. Owner grants leave attempt/writer-generation
unset but carry their binding-owned claim generation. Delegated grants pair attempt and writer
generation; branch claim generation equals writer generation. No Actor header, copied Session,
URL or client issuance material supplies authority. Publication allows exactly one stored branch
creation or trusted fast-forward. Repository identity is case-insensitive, branch identity is
case-sensitive. Default, previous defaults and protected patterns override the allowlist. Tags,
notes, deletion, multiple refs, rewind/divergence, missing ancestry/metadata and LFS uploads deny.
Policy versions increase by one and preserve observed defaults; no default-release API exists.

One dedicated caller-owned connection retains policy → tree authority → publication → runtime →
credential locks through dispatch/outcome; admission and row transactions follow. No transaction
spans metadata, GetSession, Secret or Git I/O. Fresh metadata precedes current GetSession and SQL
revalidation. Another fresh GetSession after remote-ref discovery gates DISPATCHING.

Quarantine seed discovery/upload-pack uses a request-local fixed upstream read facade. Each read
revalidates the original stamp without substituting another issuance. The facade has no receive
port and reuses an existing dispatch connection. Spooling and CPU/object validation remain outside
locks. Production construction requires this factory; P1 parsing/pack/body/resource rules remain.

## Durable uncertainty and cleanup

The existing publication ledger stores immutable request/update/version/body/association/stamp
and measured validation facts before committing DISPATCHING. Transitions remain PENDING →
DISPATCHING/REJECTED and DISPATCHING → CONFIRMED/REJECTED/UNKNOWN. Identity disagreement, repeated
dispatch and terminal reopening deny. Record/transition require the active request context and
never acquire another pool connection. The shielded outcome retains its connection and locks.

P1's validated unpack and sole matching ref receipt supplies confirmation/rejection. Stored
receipts contain bounded classification, ref, validation flag and failure code, not raw progress
or error text. HTTP 200, local validation or remote SHA equality cannot prove publication.
Timeout/cancellation/partial upload/malformed or lost reply/receipt persistence failure retains
UNKNOWN or committed DISPATCHING. No automatic resend or uncertainty resolution occurs.

Unresolved writes fence immutable owner/repository/branch across grant IDs, rotations, restarts
and successors. Reservation/release, settlement, supersession, replacement and destructive
cleanup consult the same fence. Cancellation revokes both purposes and may record confirmed
native deletion, while retaining source identity, claim and pending operation. Task publication
snapshots include `git-push:<grant>:<request>`. Invalid historical scope/evidence is a hold.

Revocation is unconditional after flags are disabled. Short caller transactions queue independent
cleanup tombstones before terminal/archive/replacement mutation. Cached values then fail despite
cleanup outages. After commit, cleanup reads the exact intended object if a UID reply was lost,
records its UID and deletes with a UID precondition. Missing objects are clean; outages, conflicts
and same-name replacements retain the hold. Audit/claim tombstones survive binding deletion.
MCP cleanup keeps its separate existing behavior.

## Remaining release gates

Native handoff/retention/checkout adapters, listeners, packaging, image provenance, GitOps,
old-session inventory/drain and actual Actor/provider/cache qualification remain separate gates.
Runtime must support frozen refs with delayed Git values and remove effective direct GitHub/PAT
paths from actors. Usable Secrets cannot be published early to work around bootstrap failures.

GetSession is a fresh observation, not atomic attestation across PostgreSQL, kagent and GitHub.
Normal fencing waits for dispatched outcome. Independent operator deletion/runtime failure
cannot recall an already dispatched push; operators must fence Mainloop first. GitHub admin
default-rename is also an external race. Source fixtures establish no live enforcement or MVP
readiness.
