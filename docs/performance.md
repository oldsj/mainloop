# Development workspace performance

Status: measured findings and proposed tuning, recorded 2026-10-10. Implemented
suite changes are in [#153](https://github.com/oldsj/mainloop/pull/153); workspace
rollout and gVisor qualification are separate.

## Summary

**Measured:** the full backend suite fell from **531.211 s to 295.236 s** on native
x86_64 Linux, a **44.4% reduction**, with all 1,449 original test IDs retained and
eight tests added. Repeated PostgreSQL work, durable writes and asyncio debug
stack collection account for substantial costs. Native per-file fsync was also
expensive on arm64, before adding sandbox overhead.

**Measured:** in the arm64 gVisor workspace, CPU-bound work took about **1.3×**
native arm64 time. Syscall-heavy flows took **8–38×**: process spawn 38×,
small-file creation and deletion about 28×, `uv sync` 28×, Python startup 14×,
and Git operations 8–12×. Fsync-bound flows took about 2× (1.5× for a sequential
write with one fsync; 2.2× for per-file fsync).

**Measured implication:** these flow measurements indicate that the backend
suite's workspace cost is dominated by subprocess and file-metadata work,
including real Git subprocesses and loopback servers, rather than CPU work.
This is an inference from the measured flows and native suite profile, not a
full-suite workspace result. Full-suite workspace qualification is in progress;
a matched workspace-versus-CI slowdown is not established. The faster native
suite has only 1.83× headroom under its 540-second cap.

## Recommendations

1. **Proposed:** cut process spawns and file churn in tests and checks. Reuse Git
   fixtures and avoid per-test subprocesses where the test is not about them.
2. **Proposed:** keep dependency caches and virtual environments warm in the
   workspace image.
3. **Proposed; kagent change in review:** add a bounded post-turn delay so long
   checks can finish instead of being frozen between turns.
4. **Proposed:** pass the sandbox CA to Node/npm with `NODE_EXTRA_CA_CERTS` when
   tools strip environment variables. The benchmark harness's online warm-up
   failed certificate validation.

**Proposed decision:** do not pursue gVisor flag tuning or KVM now. KVM is
unavailable in the current workspace environment, and flag changes require fork
plumbing for an unproven gain.

**Implemented in [#153](https://github.com/oldsj/mainloop/pull/153):** use
`make test-backend` with a disposable PostgreSQL database. The changes combine
test-only durability settings, a migrated template cloned per class, shorter
asyncio creation stacks with debug checks retained, and less successful-request
console logging. The scratch database helper still needs an image rollout before
installed workspace helpers change.

**Implemented in [#156](https://github.com/oldsj/mainloop/pull/156):** runner
lifecycle and diagnostics gaps are closed, and test, fixture and discovery
deadlines rose from 30 to 60 seconds. The foreground suite cap stays at 540
seconds plus at most five seconds of cleanup. A
15-minute CI job cap would allow setup time without lengthening the foreground
command. If workspace qualification exceeds its cap, optimize further or use
deterministic module shards whose combined test-ID inventory preserves coverage.

## Measurements

### Environments and limits

**Measured native flow baselines:** three successful samples per flow, reported
as median wall seconds. The x86_64 run used Linux, 16 CPUs, ext4 storage, Python
3.13.15 and PostgreSQL 18.6, with another backend worker active. The arm64 run used
the workspace image in a native pod limited to 6 CPU / 8 GiB. These are different
CPU/storage environments, not an isolated architecture comparison.

**Measured setup inventory supplied with the benchmark:** a single arm64 worker
node with 8 CPU / 16 GiB, and one gVisor sandbox worker limited to 6 CPU / 8 GiB
shared by all workspaces, requesting only 250m CPU / 1 GiB. Whether the node is a
VM or bare metal is unknown. The supplied runtime uses the September 2, 2026
runsc nightly. This document adds no deployment or cluster verification.

**Measured flow method:** installs used fresh environments with warm offline
caches; checkout was a local shallow clone, not a network fetch. Times exclude
cache warming, scratch checkout preparation, harness startup and PostgreSQL
initialization/start/stop. PostgreSQL rows include imports and 112 ledger tests,
not the full suite. Lint checked four Python files with one job and results cache
off. The CPU row is a synthetic eight-assertion recursive Fibonacci(32) unittest.

### Native flows and gVisor workspace measurements

**Measured:** native columns and successful gVisor flows are median wall seconds.
The gVisor run used the arm64 workspace image with 6 CPUs, in sequential chunks
inside a real Mainloop workspace. The harness labels its in-workspace execution
"native" because it does not launch another sandbox; the workspace itself runs
under gVisor. Ratios below compare gVisor against the native arm64 workspace-image
baseline, using the displayed timings; they are not isolated gVisor overhead
measurements with every environmental factor controlled.

| Flow                                             | Native x86_64 (s) | Native arm64 workspace image (s) | gVisor workspace (s) | gVisor / native arm64 |
| ------------------------------------------------ | ----------------: | -------------------------------: | -------------------: | --------------------: |
| Shallow checkout                                 |             0.208 |                            0.167 |                2.072 |                 12.4× |
| `git status --short`                             |             0.017 |                            0.016 |                0.170 |                 10.6× |
| `git log -1000 --stat` (161 available commits)   |             0.288 |                            0.202 |                1.662 |                  8.2× |
| Fresh `uv sync --frozen`                         |             0.152 |                            0.145 |                4.066 |                 28.0× |
| Python package import (`uv run --no-sync`)       |             0.024 |                            0.026 |                0.356 |                 13.7× |
| Fresh `pnpm install --frozen-lockfile`           |             1.440 |                            2.293 |         Not measured |                     — |
| Frontend build                                   |             5.108 |                            6.220 |         Not measured |                     — |
| Fixed-file Trunk check                           |             4.012 |                            4.265 |         Not measured |                     — |
| Pure-Python CPU unittest                         |             1.694 |                            2.313 |                2.938 |                  1.3× |
| PostgreSQL ledger, durability defaults           |            20.984 |                           21.091 |         Not measured |                     — |
| PostgreSQL ledger, three durability settings off |            15.161 |                           17.789 |         Not measured |                     — |
| Create 10,000 files, 128 bytes each              |             0.456 |                            0.203 |                5.885 |                 29.0× |
| Create + fsync 10,000 files, one fsync per file  |            63.853 |                           14.740 |               31.988 |                  2.2× |
| Delete 10,000 files (creation excluded)          |             0.173 |                            0.132 |                3.724 |                 28.2× |
| Sequential 128 MiB write, one final fsync        |             0.126 |                            0.079 |                0.115 |                  1.5× |
| Spawn 500 external processes                     |             0.360 |                            0.326 |               12.397 |                 38.0× |

**Measured limitations:** `pnpm install`, frontend build and lint were not
measured. The harness strips CA and proxy environment variables; its online
warm-up hit npm's "self-signed certificate in certificate chain" error, and the
chunk reached its 540-second cap. Neither PostgreSQL flow was measured because
the harness refuses a root guest. **Implemented:** the repository's
`dev-postgres` helper handles root by dropping to an unprivileged user; the
harness failure does not establish a workspace PostgreSQL limitation.

**Measured:** per-file fsync increased file creation time about 140× on x86_64
and 73× on arm64. Disabling the three PostgreSQL durability settings together
reduced the ledger module by 27.8% and 15.7%, respectively. These results do not
isolate an individual setting or establish a gVisor cause. Native x86_64 process-tree
PSS peaks were 41 MiB for checkout and 884 MiB for frontend build, sampled at
100 ms; they exclude kernel page cache and are not cgroup memory peaks.

### Full backend suite before and after

**Measured:** single observations on native x86_64 Linux, Python 3.13.15 and an
owned PostgreSQL 16.15 Docker service. The final run used a fresh CI-style 512 MiB
PGDATA tmpfs, `initdb --no-sync`, `fsync=off`, `synchronous_commit=off`,
`full_page_writes=off` and `max_wal_size=128MB`. These settings are limited to
throwaway test databases. This was a local exercise of the CI configuration,
not a hosted CI run or a gVisor workspace run.

| Run                                                | Tests | Wall seconds | Measured outcome |
| -------------------------------------------------- | ----: | -----------: | ---------------- |
| Default disk-backed PostgreSQL                     | 1,449 |      531.211 | Pass             |
| Durability off, disk-backed, original fixtures     | 1,454 |      433.546 | Pass             |
| Final capped command, tmpfs and structural changes | 1,457 |      295.236 | Pass, zero skips |

**Measured:** settings alone saved 97.665 seconds. Summed class/module fixture
time fell from 45.170 to 3.942 seconds, but storage/settings changed too, so this
is not solely template-cloning savings. The old fixture already used a database
per class and a pool per test; it did not migrate a database for every test.
A debug-stack diagnostic probe fell from 113,406 to 14,110 stat calls and from
2.019 to 0.321 seconds in stack extraction; this is a native probe, not sandbox proof.

**Measured remaining costs:** the slowest final modules were task application
integration (54.791 s), handoff (41.016 s), Git credentials (29.409 s), provisioning
(26.338 s) and HTTP Git transport (20.269 s). They exercise repeated SQL authority
checks and writes, crash/reconnection boundaries, real Git subprocesses and
loopback HTTP servers. Intentional observation delays, contention checks,
security matrices and timeout/recovery assertions remain. The slowest individual
accepted test took 6.202 seconds.

## Details and references

### Runner bounds

**Implemented in [#153](https://github.com/oldsj/mainloop/pull/153):** the runner requires a scratch
`MAINLOOP_TEST_DATABASE_URL` rather than silently skipping PostgreSQL coverage.
Its configured deadlines are now 60 seconds per test/fixture/discovery (raised
from 30 in [#156](https://github.com/oldsj/mainloop/pull/156)) and 540 seconds
for the suite; suite timeout exits 124, fatal phase timeout exits 1, and missing
database URL exits 2. `TEST_ARGS` selects unittest names;
`MAINLOOP_TEST_TIMINGS` saves timing JSON. Sync dependencies first with
`uv sync --frozen --python 3.13`.

**Implemented in [#156](https://github.com/oldsj/mainloop/pull/156):** review of
#153 found four lifecycle gaps: suites returned by `load_tests` bypassed fixture
deadlines, async runner shutdown ran after the test timer was cancelled, suite
termination could truncate the stack dump, and cancellation during launch could
escape process-group cleanup. All four are fixed with regression tests; the
measured native run passed 1,463 tests in 295.192 seconds. The 60-second bound
gives 9.67× headroom over the slowest native test, versus 4.84× at 30 seconds.
It is not yet qualified on gVisor arm64.

### Runtime capacity and filesystem controls

**Implemented in the inspected fork snapshot:**
[kagent's dependency pin](https://github.com/oldsj/kagent/blob/18932910776baf9d590c768ac8a6ed7b594f82d4/go/go.mod)
is Substrate v0.4.0-alpha1. The benchmark inspection found 1,000 default actor
slots per worker and an
[ActorTemplate translator](https://github.com/oldsj/kagent/blob/18932910776baf9d590c768ac8a6ed7b594f82d4/go/core/internal/substrate/actor_template.go)
that omits actor resources. The newer 1.x tuning guide's one-actor-per-worker
model must therefore be checked against the deployed fork.

**Measured binary defaults:** the exact amd64 nightly reports systrap,
`overlay2=root:self`, `directfs=true`, `file-access=exclusive` and
`file-access-mounts=shared`. Its root overlay is file-backed, not memory-backed.
This is binary flag evidence, not an observation inside the live arm64 sandbox.
All rootless filesystem variants were blocked at startup by AppArmor user-namespace
policy, so failed-launch memory is not workload memory.

**Proposed; deferred:** overlay, directfs, mount-access and dentry-cache
comparisons would need runtime-fork plumbing and workload memory measurements.
Their gain is unproven, so they are not current recommendations. KVM is unavailable
in the current workspace environment. Any future deployment changes must use
GitOps; test-only durability changes must remain limited to disposable PGDATA.

### Reading

- [kagent: Tune Agent Substrate](https://kagent.dev/docs/kagent/1.x/operations/tune-agent-substrate/):
  **proposed guidance** for sizing pools by simultaneous turns and reserving
  worker resources; version differences above apply.
- [Substrate architecture](https://github.com/agent-substrate/substrate/blob/main/docs/architecture.md):
  **proposed targets** of 100 ms p95 activation and 1,000 wakeups per second,
  not measurements for this setup.
- [gVisor production guide](https://gvisor.dev/docs/user_guide/production/):
  **proposed platform guidance** for KVM on bare metal and systrap in VMs.
- [gVisor performance guide](https://gvisor.dev/docs/architecture_guide/performance/):
  **proposed interpretation** of syscall/filesystem overhead versus compute;
  historical benchmark data is not a slowdown estimate for these workspaces.
- [gVisor filesystem guide](https://gvisor.dev/docs/user_guide/filesystem/):
  **proposed tuning guidance** for directfs, overlays, dentry cache and mount
  access modes, including the exclusive-cache ownership requirement.

**Measured evidence provenance:** the tables summarize the native flow benchmark,
its arm64 native workspace-image baseline, the chunked gVisor workspace run, and
the backend test-speed result and
independent review recorded on 2026-10-10. Raw logs remain private task artifacts.
Pending work is matched gVisor memory measurement, full-suite workspace qualification,
concurrency/throttling measurements, hosted exact-commit CI and helper-image rollout.
