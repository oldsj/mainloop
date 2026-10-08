# Task handoff

Status: partial S3 coordinator source candidate, uninstalled. Offline fake and
PostgreSQL regression evidence is not live runtime proof. Receipt storage and
schema upgrade have offline qualification; cleanup remains uninstalled. Live retry/reassign
remain unavailable until runtime fencing, preview closure and exact readiness are
qualified. No automatic provider fallback occurs.

A retry keeps the selected provider; explicit reassignment chooses the target.
Both create a fresh attempt, native history and grant, preserving the task,
feature branch, environment and caller instructions. Request IDs bind to the
canonical digest and current version/attempt. Replays reconcile the original
operation rather than starting another writer.

A completed owner cancellation remains terminal across dispatcher checkpoints
and coordinator restarts, including a fenced source or a rejected no-start with
no current attempt. Successor enrollment revalidates task status and version in
the admission transaction. Stale handoff/error records preserve the terminal task
projection; they cannot create a new attempt, claim, grant or first brief.

Any source that could have edited files must commit and push before handoff.
Trusted evidence must verify the exact project repository, feature branch and
exact remote committed-tree SHA. Actor clean-status assertions and provider
notes remain untrusted; this does not attest to hidden or ignored files. Unknown Git, native or
merge dispatch prevents transfer. A definite failed no-start may use its trusted
initial ref after that ref is resolved and verified; this exception is pending
integration and is not inferred from session absence.

Source credentials must be revoked and the exact runtime confirmed quiescent,
including previews and supervisor children, before generation CAS releases the
claim. Suspension and timeouts are insufficient. Live handoff requires qualified
runtime evidence; offline fake evidence cannot qualify a live writer. Every
source submit/resume/recreate/preview path remains governed by S1 lifecycle.

The successor must use S1 provisioning, a new identity and grant, and the verified
remote SHA. Confirm actual checkout and environment before writable admission or
first brief. S1 active admission follows read-only checkout verification; fresh
grants require a scoped confirmation, then one first brief is recorded in the
existing ledger. Ordinary native actions remain gated while admission is pending.
Lost create replies reconcile the original identity. Explicit
source restoration requires confirmed target absence/fencing and fresh grants.

Continuation artifacts are deterministic and bounded. Optional provider notes
are labelled unverified. Native transcripts, pending HITL and old approval
receipts are not transferred. Publication linkage and consent belong to the
current attempt. Task operations expose confirmed steps and lineage separately
from task status.

Superseded sessions archive after 21 days and delete after two calendar months
from supersession, configurable through trusted settings. Validation uses the
earliest clamped boundary across the Gregorian 400-year cycle. Calendar arithmetic
clamps to the destination month's final day. Holds prevent cleanup; cleanup
failure retains pending state. The existing dispatcher owns reconciliation and
audit tombstones survive deletion. The full typed receipt uses an immutable
artifact and bounded attempt evidence reference, committed atomically with the
deletion timestamp. Before cleanup, a rolled-back storage probe validates the
actual schema, model and artifact readback; unsupported storage invokes no
cleanup. Existing schemas receive an idempotent typed-kind upgrade. These have
offline PostgreSQL evidence; cleanup remains unset until its runtime adapter is
qualified. Dispatcher hookup and qualified cleanup adapters are pending serialized
integration; there is no new scheduler. Explicit source-restoration orchestration
is still pending: a failed or absent target currently holds its claim and the
revoked predecessor remains fenced.

The continuation brief preserves the authoritative original owner instructions
exactly once within 16 KiB UTF-8. Additional manifest context remains durable.
A scoped native task/artifact reader is still required before integration can
deliver that context; owner REST reads are not an agent capability.
