# Git publication authority

## Implemented source, default off

`GIT_TRANSPORT_ENABLED` and `PUSH_GATE_ENABLED` default to `false`. Source includes injectable
read/push ASGI applications, PostgreSQL authority and native enrollment callers. Tracked GitOps
manifests now include the `mainloop.git_app` backend-image sidecar, port-80 Services
`mainloop-git-read` and `mainloop-git-push` in `mainloop`, and a separate ingress NetworkPolicy
allowing only `ate-system` pods labelled `app=atenet-egress` to listener ports 8003 and 8004.
The sidecar shares one database pool, kagent client and pack-validation slot across both ports;
repository upstreams are request-local. Its disk-backed, bounded emptyDir holds quarantine data.
Read requires `GIT_TRANSPORT_ENABLED`; push also requires `PUSH_GATE_ENABLED`. Disabled listeners
refuse every HTTP request before authentication, token minting or upstream traffic. With Git
transport off, startup needs no database or GitHub App connection. Flags remain off by default.
These are installed source/manifests, not a deployed image or live Actor containment proof.
Real local PostgreSQL with fake kagent/Kubernetes and a fixed local Git upstream proves source
behavior only.

Trusted outbound clients use the existing GitHub App authenticator outside the actor. Each Git
operation obtains a cached installation token scoped to the authenticated repository with only
`contents: read` for upload-pack (including quarantine seeds), or `contents: write` for
receive-pack/discovery. Tokens are minted only after current dispatch authority succeeds, never
from an Actor-supplied credential. The existing “GitHub App not installed on <repo>” refusal
remains fail-closed. Mint failures never dispatch Git traffic; credentials and upstream exception
text never become Git responses or logs. No PAT is used by the production listeners.
Actor capabilities are independent for the exact origins
`http://mainloop-git-read.mainloop.svc.cluster.local` and
`http://mainloop-git-push.mainloop.svc.cluster.local`. Neither purpose grants MCP or owner API
access. No new GitHub principal is introduced. Legacy random hash-only grants
remain compatible when Git transport is disabled; they publish no Git Secret and cannot
authenticate on the new listener. Storing a Session UUID no longer enrolls a grant.

## Frozen enrollment and publication

After workspace/attempt admission commits, Mainloop freezes the original CreateSession identity,
owner/project/repository/branch, Agent, checkout, accepted development environment,
MCP/read/optional-push references, attempt/claim generation and reserved push version. Default
and protected workspaces reserve read only; coordination sessions receive no Git plan. Historical
or dispatched bindings without a plan cannot be retrofitted. Missing dispatch history is a hold.

With the push gate enabled, a workspace writer plan needs the project's default branch. When the
cached value is empty (as on every freshly imported project), Mainloop reads the repository's
default branch through the repository-scoped GitHub App client before reserving any references,
whatever checkout ref was requested, and stores it through the same atomic project-metadata and
protected-branch-policy update the owner metadata refresh uses. If GitHub cannot supply it, no
plan or enrollment is frozen and creation holds: a task attempt stays `creating` with evidence
`git-hold:default_branch_unavailable` and the next reconciliation pass retries. Mainloop never
guesses a default branch and never freezes a feature-branch writer into a read-only plan because
metadata is missing. A writer whose plan has no push reference records why on its task attempt
as `git-push-absent:<reason>` (for example `default_branch` or `protected_branch`).

New plans spell the Git read and push reference header as lowercase `authorization`, the exact
form kagent's preparation check counts; it requires exactly one read and one push reference at
the configured origins, each with Secret key `authorization`. The MCP reference is unchanged.
Plans frozen earlier keep their original bytes and digest; publication and cleanup reuse each
plan's frozen references.

With both gates enabled, new task and owner workspace checkouts resolve to full commit SHAs
through the repository-scoped GitHub App commits endpoint (`contents: read`) before admission
rows and the create plan are frozen. Empty refs resolve GitHub's current default branch; explicit
branches, tags and SHAs are verified. An unavailable ref refuses creation. Retries reuse the
frozen SHA; handoff successors retain their verified `remote_sha`, and existing refs are untouched.

Issuance uses HMAC-SHA256 with the existing `AGENT_TOKEN_KEY`, a versioned Git domain, independent
purpose and immutable issuance/version/binding/create identity. Read values start with `gread_`;
push values retain `push_`. PostgreSQL stores hashes and references, never capability or App-token bytes.
Missing keys and recovery hash mismatches fail closed, without rotating the issuance.

The complete tuple and dispatch marker commit before CreateSession bytes. Usable Git Secrets
remain absent. Mainloop runs a durable, owned non-turn Suspend/Resume warmup on the same Session,
then obtains a fresh GetSession. READY, Create, List and Resume replies supply no Git association.
Confirmation requires the exact stored Session/context/Agent/checkout/environment, prepared
revision, reported composition and current active field-20 generation/atespace/Actor name/UID.
The confirmed association is immutable. A changed generation or UID refuses old capabilities.

Read publication follows confirmation; delegated creating targets receive read only. Push follows
active/current admission. Native send confirms required publications before external turn bytes.
With both flags enabled, workspace setup additionally requires a nonhistorical confirmed
workspace preparation receipt. Mainloop commits one stable preparation action and its original
request before dispatch, using only the frozen create plan, confirmed runtime association and
authoritative binding role. The `prepare_receipt` row initially contains only that local
`original` request reservation, without a remote classification; it grants no preparation
authority. Lost replies with no receipt retry identical bytes and the same action. Pending or
uncertain receipts are polled without reissuing Prepare; confirmed receipts never open another
challenge. ALREADY_EXISTS, definite failure, receipt identity/profile disagreement and historical
receipts hold the enrollment for Session replacement. A failed preparation keeps a bounded,
secret-free reason: `prepare_receipt.failure` records Mainloop's failure code, the gRPC status
when kagent refused Prepare, and Mainloop's own count of kagent-acceptable Git references
(`git_refs: {read, push}`); the task attempt gains matching evidence such as
`git-prepare-failed:git_prepare_failed:grpc=9:git-refs read=1 push=0`. Runtime error text is
never stored. A non-READY Session or an operation in
progress holds preparation without failing its durable state. kagent temporarily projects
historical receipts during ordinary suspension/resume; only a fresh settled READY observation
of the original runtime can classify history as terminal. The authoritative binding role alone
selects the profile: owner `agent` maps to kagent `agent` with setup digest
`a5fb1bb1e406ff7937b9d9e2e862dff43925d4df54d4bdd9fb010d7e2825ccb3` pinned to Standing at
kagent `796e90b5`; supervisor and child mappings and digests are unchanged. Every native
send requires durable `prepare_state=confirmed` for its current issuance/create identity when
both flags are enabled. A binding's previous turn count grants no preparation authority.
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

Shallow clients (kagent's default checkout is depth 1) send `shallow <oid>` lines before the
command list. The gate accepts distinct, lowercase, non-zero SHA-1 lines only in that position and
forwards them upstream inside the unchanged body. They grant nothing: ref policy, old-oid,
fast-forward and fsck checks run against the quarantine's full-history seed of the target and
default branches, so a push whose history connects only through a client graft is refused.
Push certificates remain unsupported.

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

Native handoff/retention/checkout adapters, published packaging/image provenance, GitOps rollout,
old-session inventory/drain and actual Actor/provider/cache qualification remain separate gates.
Runtime must support frozen refs with delayed Git values and remove effective direct GitHub/PAT
paths from actors. Usable Secrets cannot be published early to work around bootstrap failures.

GetSession is a fresh observation, not atomic attestation across PostgreSQL, kagent and GitHub.
Normal fencing waits for dispatched outcome. Independent operator deletion/runtime failure
cannot recall an already dispatched push; operators must fence Mainloop first. GitHub admin
default-rename is also an external race. Source fixtures establish no live enforcement or MVP
readiness.
