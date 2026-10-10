#!/usr/bin/env bash
# Serial Mainloop workload benchmark. No repository writes, sudo, Docker or Kubernetes.
# Requires uv, git, pnpm, trunk and PostgreSQL binaries (PG_BIN/PG_SHARE/PG_LIB).
# Native-only in a real workspace: ./bench.sh --native-only --repo /workspace/repo
# Results: samples.jsonl, summary.md, metadata.json, setup.log and per-run logs.
# Fresh dependency targets, warm isolated caches; scratch is removed on exit.
set -euo pipefail
BENCH_SCRIPT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/$(basename -- "${BASH_SOURCE[0]}")
export BENCH_SCRIPT
export TMPDIR="${BENCH_TMPDIR:-${BENCH_SCRATCH_BASE:-${HOME}/.cache/gvisor-bench}/tmp}"
mkdir -p -- "$TMPDIR"
exec uv run --no-project --python "${BENCH_PYTHON:-3.13}" python - "$@" <<'PY'
import argparse
import atexit
import datetime
import hashlib
import json
import os
from pathlib import Path
import platform
import shlex
import shutil
import socket
import statistics
import subprocess
import sys
import tempfile
import threading
import time

FLOWS = ["checkout", "git_status", "git_log", "uv_sync", "python_startup", "pnpm_install", "frontend_build",
         "lint", "cpu_unittest", "postgres_default", "postgres_relaxed",
         "small_create", "small_fsync", "small_delete", "sequential_fsync", "spawn"]
IO_FLOWS = {"uv_sync", "pnpm_install", "postgres_default", "postgres_relaxed", "small_create", "small_fsync", "small_delete"}
PROFILES = {"defaults": [], "root_self": ["--overlay2=root:self"],
            "root_memory": ["--overlay2=root:memory"], "overlay_none": ["--overlay2=none"],
            "directfs_on": ["--directfs=true"], "directfs_off": ["--directfs=false"],
            "root_exclusive": ["--file-access=exclusive"], "root_shared": ["--file-access=shared"],
            "mount_exclusive": ["--file-access-mounts=exclusive"], "mount_shared": ["--file-access-mounts=shared"]}
FILES = ["backend/src/mainloop/runtime/kagent_client.py",
         "backend/src/mainloop/runtime/agent_identity.py",
         "backend/src/mainloop/providers.py", "backend/tests/runtime/test_task_contracts.py"]
CPU = '''import unittest
def fibonacci(n):
    return n if n < 2 else fibonacci(n-1) + fibonacci(n-2)
class CPUOnly(unittest.TestCase):
    def test_recursive_integer_work(self):
        for _ in range(8):
            self.assertEqual(fibonacci(32), 2178309)
'''
parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--repo", type=Path, default=Path.cwd())
parser.add_argument("--output", type=Path, default=Path(os.environ["BENCH_SCRIPT"]).parent)
parser.add_argument("--native-only", action="store_true")
parser.add_argument("--runs", type=int, default=3)
parser.add_argument("--pnpm-version", default="9.15.9", help="Match CI's pnpm 9; isolated npm install if needed")
parser.add_argument("--matrix", action="store_true", help="Compare default filesystem flags with one-option variants on I/O flows")
parser.add_argument("--probe-only", action="store_true", help="Only test rootless configuration startup; no workloads or package installs")
parser.add_argument("--flows", default=",".join(FLOWS))
parser.add_argument("--worker", nargs=3, metavar=("FLOW", "SCRATCH", "SOURCE"), help=argparse.SUPPRESS)
args = parser.parse_args()

def execute(command, **kw):
    print("$ " + shlex.join([str(c) for c in command]), flush=True)
    return subprocess.run([str(c) for c in command], check=True, **kw)

def cgroup_memory():
    try:
        entry = next(line for line in Path("/proc/self/cgroup").read_text().splitlines() if line.startswith("0::"))
        root = Path("/sys/fs/cgroup") / entry[3:].lstrip("/")
        return {name: (root / name).read_text().strip() for name in ("memory.current", "memory.peak", "memory.max") if (root / name).exists()}
    except (OSError, StopIteration):
        return {}

def monitored(command, env, log):
    # Sample process-tree PSS, including the native DB or runsc/gofer if reachable.
    # This excludes kernel page cache; it is not a cgroup memory high-water mark.
    process = subprocess.Popen([str(c) for c in command], env=env, stdout=log, stderr=log, start_new_session=True)
    peak = [0]
    known = {process.pid}
    stop = threading.Event()
    def sample():
        while not stop.is_set():
            parents = {}
            for path in Path("/proc").iterdir():
                if not path.name.isdigit():
                    continue
                try:
                    fields = (path / "stat").read_text().rsplit(")", 1)[1].split()
                    parents[int(path.name)] = int(fields[1])
                except (OSError, ValueError, IndexError):
                    pass
            changed = True
            while changed:
                children = {pid for pid, parent in parents.items() if parent in known}
                changed = bool(children - known)
                known.update(children)
            total = 0
            for pid in known:
                try:
                    for line in Path(f"/proc/{pid}/smaps_rollup").read_text().splitlines():
                        if line.startswith("Pss:"):
                            total += int(line.split()[1])
                            break
                except (OSError, ValueError):
                    pass
            peak[0] = max(peak[0], total)
            stop.wait(0.1)
    monitor = threading.Thread(target=sample, daemon=True)
    monitor.start()
    code = process.wait()
    stop.set()
    monitor.join()
    return code, peak[0] / 1024 if peak[0] else None

def worker(flow, scratch, source):
    scratch, source = Path(scratch), Path(source)
    repo = args.repo.resolve()
    backend = source / "backend"
    uv = ["uv", "run", "--no-sync", "--offline", "--python", sys.executable, "python"]
    io = scratch / "io"
    io.mkdir(exist_ok=True)
    notes = {}
    pg_data = scratch / "pg-data"
    pg_started = False
    pgctl = None
    def stop_pg_on_exit():
        if pg_started and (pg_data / "postmaster.pid").exists():
            subprocess.run([str(pgctl), "-D", str(pg_data), "-m", "fast", "-w", "stop"], check=False)
    atexit.register(stop_pg_on_exit)
    if flow == "uv_sync":
        os.environ["UV_PROJECT_ENVIRONMENT"] = str(scratch / "venv")
    if flow.startswith("postgres_"):
        if os.geteuid() == 0:
            raise RuntimeError("PostgreSQL requires a non-root guest; use the single-UID OCI runner")
        pg_bin = os.environ.get("PG_BIN")
        if not pg_bin:
            executable = shutil.which("initdb")
            if not executable:
                raise RuntimeError("Set PG_BIN to a directory containing initdb, pg_ctl and psql")
            pg_bin = str(Path(executable).parent)
        pgctl = Path(pg_bin) / "pg_ctl"
        init = [Path(pg_bin) / "initdb", "-D", pg_data, "--auth=trust", "--username=bench",
                "--encoding=UTF8", "--locale=C", "--no-sync"]
        if os.environ.get("PG_SHARE"):
            init += ["-L", os.environ["PG_SHARE"]]
        execute(init)
        # Port belongs to this new local instance. Never use an inherited DB URL.
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0))
            port = sock.getsockname()[1]
        options = f"-p {port} -h 127.0.0.1 -k '' -c shared_buffers=128MB -c max_connections=30"
        if flow == "postgres_relaxed":
            options += " -c fsync=off -c synchronous_commit=off -c full_page_writes=off"
        pg_started = True
        execute([pgctl, "-D", pg_data, "-l", scratch / "postgres.log", "-o", options, "-w", "start"])
        os.environ["MAINLOOP_TEST_DATABASE_URL"] = f"postgresql://bench@127.0.0.1:{port}/postgres"
        # Show the effective settings, not just requested flags.
        execute([Path(pg_bin) / "psql", os.environ["MAINLOOP_TEST_DATABASE_URL"], "-Atc",
                 "SELECT current_setting('fsync'), current_setting('synchronous_commit'), "
                 "current_setting('full_page_writes')"])
    if flow == "small_delete":
        for i in range(10_000):
            (io / str(i)).write_bytes(b"x" * 128)
    try:
        notes["cgroup_before"] = cgroup_memory()
        start = time.perf_counter()
        if flow == "checkout":
            # file:// forces the local transport to honour depth instead of copying full history.
            execute(["git", "clone", "--depth=1", repo.as_uri(), scratch / "checkout"])
            notes["checkout_commits"] = subprocess.check_output(["git", "-C", scratch / "checkout", "rev-list", "--count", "HEAD"], text=True).strip()
        elif flow == "git_status":
            execute(["git", "-C", repo, "status", "--short"], stdout=subprocess.DEVNULL)
        elif flow == "git_log":
            execute(["git", "-C", repo, "log", "-1000", "--stat"], stdout=subprocess.DEVNULL)
        elif flow == "uv_sync":
            execute(["uv", "sync", "--frozen", "--offline", "--python", sys.executable], cwd=backend)
        elif flow == "python_startup":
            execute(uv + ["-c", "import mainloop"], cwd=backend)
        elif flow == "pnpm_install":
            execute(["pnpm", "install", "--frozen-lockfile", "--offline", "--store-dir",
                     scratch.parent / "pnpm-store"], cwd=source)
        elif flow == "frontend_build":
            execute(["pnpm", "-C", "frontend", "build"], cwd=source)
        elif flow == "lint":
            execute(["trunk", "check", "--no-fix", "--cache=false", "--filter=ruff,black,isort,bandit",
                     "--jobs=1", "--ci", "--no-progress"] + FILES, cwd=source)
        elif flow == "cpu_unittest":
            execute(uv + ["-m", "unittest", "discover", "-s", scratch.parent / "cpu", "-p", "test_cpu.py", "-v"], cwd=backend)
        elif flow.startswith("postgres_"):
            execute(uv + ["-m", "unittest", "tests.runtime.test_postgres_ledger", "-v"], cwd=backend)
        elif flow in ("small_create", "small_fsync"):
            for i in range(10_000):
                with (io / str(i)).open("wb", buffering=0) as handle:
                    handle.write(b"x" * 128)
                    if flow == "small_fsync":
                        os.fsync(handle.fileno())
        elif flow == "small_delete":
            for i in range(10_000):
                (io / str(i)).unlink()
        elif flow == "sequential_fsync":
            block = b"x" * (1024 * 1024)
            with (io / "sequential").open("wb", buffering=0) as handle:
                for _ in range(128):
                    handle.write(block)
                os.fsync(handle.fileno())
        elif flow == "spawn":
            execute(["/bin/bash", "-c", "for i in $(seq 500); do /bin/true; done"])
        else:
            raise ValueError(flow)
        elapsed = time.perf_counter() - start
        notes["cgroup_after"] = cgroup_memory()
        print("BENCH_RESULT " + json.dumps({"seconds": elapsed, "notes": notes}), flush=True)
    finally:
        if pg_started:
            execute([pgctl, "-D", pg_data, "-m", "fast", "-w", "stop"])
            pg_started = False

if args.worker:
    worker(*args.worker)
    sys.exit(0)

if args.runs < 1:
    parser.error("--runs must be positive")
flows = args.flows.split(",")
if any(f not in FLOWS for f in flows):
    parser.error("Unknown flow")
repo = args.repo.resolve()
output = args.output.resolve()
output.mkdir(parents=True, exist_ok=True)
base = Path(os.environ.get("BENCH_SCRATCH_BASE", str(Path.home() / ".cache/gvisor-bench"))).resolve()
if base == Path("/tmp") or Path("/tmp") in base.parents or base == Path("/"):
    parser.error("Scratch must be a task-specific directory outside /tmp")
base.mkdir(parents=True, exist_ok=True)
scratch = Path(tempfile.mkdtemp(prefix="run-", dir=base))
source = scratch / "source"
original_env = os.environ.copy()
env = {key: original_env[key] for key in ("HOME", "PATH", "SSL_CERT_FILE", "SSL_CERT_DIR") if key in original_env}
env.update({"TMPDIR": str(scratch / "tmp"), "XDG_CACHE_HOME": str(scratch / "cache"),
            "BENCH_TMPDIR": str(scratch / "tmp"),
            "UV_CACHE_DIR": str(scratch / "uv-cache"), "UV_PROJECT_ENVIRONMENT": str(scratch / "venv"),
            "UV_PYTHON_DOWNLOADS": "never", "PYTHONDONTWRITEBYTECODE": "1", "GIT_OPTIONAL_LOCKS": "0",
            "CI": "true", "AGENT_TOKEN_KEY": "benchmark-fixture-key", "LC_ALL": "C.UTF-8",
            "BENCH_SCRIPT": str(Path(os.environ["BENCH_SCRIPT"]).resolve())})
for key in ("PG_BIN", "PG_SHARE", "PG_LIB"):
    if key in original_env:
        env[key] = original_env[key]
if env.get("PG_LIB"):
    env["LD_LIBRARY_PATH"] = env["PG_LIB"]
(scratch / "tmp").mkdir()
(scratch / "cpu").mkdir()
(scratch / "cpu/test_cpu.py").write_text(CPU)
os.environ.update(env)
script = Path(env["BENCH_SCRIPT"])
already_gvisor = Path("/proc/gvisor").exists() or "gvisor" in Path("/proc/version").read_text().lower()
native_only = args.native_only or already_gvisor
runsc = shutil.which("runsc")
flags = ["--root=" + str(scratch / "runsc"), "--network=host", "--platform=systrap",
         "--ignore-cgroups=true"]
profiles = list(PROFILES) if args.matrix else ["defaults"]
availability = {}
samples = []
metadata = {"date": datetime.datetime.now(datetime.UTC).isoformat(), "host": platform.node(),
            "kernel": platform.release(), "machine": platform.machine(), "cpus": os.cpu_count(),
            "repo": str(repo), "commit": subprocess.check_output(["git", "-C", repo, "rev-parse", "HEAD"], text=True).strip(),
            "runs": args.runs, "native_only": native_only, "already_gvisor": already_gvisor,
            "scratch": str(scratch), "files": FILES, "runsc_flags": flags,
            "profiles": {name: PROFILES[name] for name in profiles},
            "script_sha256": hashlib.sha256(script.read_bytes()).hexdigest(),
            "security_profile": Path("/proc/self/attr/current").read_text().strip() if Path("/proc/self/attr/current").exists() else None,
            "disk_free_bytes": shutil.disk_usage(base).free}

def version(command):
    return subprocess.run(command, capture_output=True, text=True, env=env).stdout.strip()

def oci(command, identifier, profile="defaults"):
    # Native rootless OCI: single UID/GID maps retain a non-root PostgreSQL user.
    # Read-only host root, one writable scratch bind. Root flags do not change
    # the bind mount: the matrix also includes file-access-mounts variants.
    bundle = scratch / ("bundle-" + identifier)
    bundle.mkdir()
    spec = {"ociVersion": "1.2.0", "root": {"path": "/", "readonly": True},
            "process": {"terminal": False, "user": {"uid": os.getuid(), "gid": os.getgid()},
                        "args": [str(c) for c in command], "env": [f"{k}={v}" for k,v in env.items()],
                        "cwd": str(repo), "noNewPrivileges": True,
                        "rlimits": [{"type": "RLIMIT_NOFILE", "hard": 65536, "soft": 65536}]},
            "mounts": [{"destination": str(scratch), "type": "bind", "source": str(scratch),
                        "options": ["rbind", "rw"]},
                       {"destination": "/proc", "type": "proc", "source": "proc",
                        "options": ["nosuid", "noexec", "nodev"]}],
            "linux": {"namespaces": [{"type": n} for n in ("pid", "ipc", "uts", "mount", "user")],
                      "uidMappings": [{"containerID": os.getuid(), "hostID": os.getuid(), "size": 1}],
                      "gidMappings": [{"containerID": os.getgid(), "hostID": os.getgid(), "size": 1}]}}
    (bundle / "config.json").write_text(json.dumps(spec))
    return [runsc] + flags + PROFILES[profile] + ["run", "--bundle", bundle, identifier]

try:
    metadata["versions"] = {"uv": version(["uv", "--version"]), "pnpm": version(["pnpm", "--version"]),
                            "python": sys.version, "node": version(["node", "--version"])}
    unavailable = "Native-only requested or existing gVisor detected" if native_only else "runsc is not installed"
    gvisor_ok = False
    if runsc and not native_only:
        metadata["versions"]["runsc"] = version([runsc, "--version"])
        do_probe = [runsc] + flags + ["--rootless", "do", "--force-overlay=false", "/bin/true"]
        with (output / "runsc-do-probe.log").open("w") as log:
            log.write(shlex.join(do_probe) + "\n"); log.flush()
            result = subprocess.run(do_probe, env=env, stdout=log, stderr=log)
            metadata["do_probe_exit"] = result.returncode
        for profile in profiles:
            probe = oci(["/bin/true"], "bench-probe-" + profile.replace("_", "-"), profile)
            filename = "runsc-oci-probe.log" if profile == "defaults" else f"runsc-{profile}-probe.log"
            with (output / filename).open("w") as log:
                log.write(shlex.join([str(c) for c in probe]) + "\n"); log.flush()
                result = subprocess.run(probe, env=env, stdout=log, stderr=log)
            availability[profile] = {"available": result.returncode == 0, "exit_code": result.returncode,
                                     "log": filename, "flags": PROFILES[profile], "workload_memory_mib": None}
        gvisor_ok = availability["defaults"]["available"]
        metadata["oci_probe_spec"] = json.loads((scratch / "bundle-bench-probe-defaults/config.json").read_text())
        unavailable = "Rootless startup failed; see runsc-do-probe.log and runsc-oci-probe.log"
        print("gVisor: " + ("rootless single-UID OCI available" if gvisor_ok else unavailable), flush=True)
    metadata["gvisor_available"] = gvisor_ok
    metadata["gvisor_unavailable_reason"] = None if gvisor_ok else unavailable
    (output / "configurations.json").write_text(json.dumps(availability, indent=2) + "\n")
    if args.probe_only:
        sys.exit(0)
    with (output / "setup.log").open("w") as log:
        def setup(command, cwd=None):
            log.write("$ " + shlex.join([str(c) for c in command]) + "\n"); log.flush()
            subprocess.run([str(c) for c in command], cwd=cwd, env=env, stdout=log, stderr=log, check=True)
        setup(["git", "clone", "--shared", "--no-checkout", repo, source])
        setup(["git", "-C", source, "checkout", "--detach", metadata["commit"]])
        need_pnpm = any(flow in flows for flow in ("pnpm_install", "frontend_build"))
        need_uv = any(flow in flows for flow in ("uv_sync", "python_startup", "cpu_unittest", "postgres_default", "postgres_relaxed"))
        if need_pnpm and version(["pnpm", "--version"]) != args.pnpm_version:
            setup(["npm", "install", "--prefix", scratch / "tools", "--cache", scratch / "npm-cache",
                   "--ignore-scripts", "--no-audit", "--no-fund", "pnpm@" + args.pnpm_version])
            env["PATH"] = str(scratch / "tools/node_modules/.bin") + os.pathsep + env["PATH"]
            metadata["versions"]["pnpm"] = version(["pnpm", "--version"])
        if need_uv:
            setup(["uv", "sync", "--frozen", "--python", sys.executable], source / "backend")
        if need_pnpm:
            setup(["pnpm", "install", "--frozen-lockfile", "--store-dir", scratch / "pnpm-store"], source)
        if "lint" in flows:
            setup(["trunk", "check", "--no-fix", "--cache=false", "--filter=ruff,black,isort,bandit",
                   "--jobs=1", "--ci", "--no-progress"] + FILES, source)
    print("Caches warm; starting serial timings", flush=True)
    (output / "samples.jsonl").write_text("")
    for flow in flows:
        legs = [("native", "native")]
        if not native_only:
            legs += [("gvisor", name) for name in (profiles if flow in IO_FLOWS else ["defaults"])]
        for leg, profile in legs:
            for run in range(1, args.runs + 1):
                load = subprocess.check_output(["uptime"], text=True).strip() if shutil.which("uptime") else "loadavg " + Path("/proc/loadavg").read_text().strip()
                row = {"flow": flow, "leg": leg, "profile": profile, "run": run, "uptime": load,
                       "utc": datetime.datetime.now(datetime.UTC).isoformat()}
                if leg == "gvisor" and not availability.get(profile, {}).get("available"):
                    row.update(status="unavailable", reason=unavailable)
                else:
                    work = scratch / f"{flow}-{leg}-{run}"
                    work.mkdir()
                    trial_source = source
                    if flow == "pnpm_install":
                        trial_source = work / "install-source"
                        subprocess.run(["git", "clone", "--shared", "--quiet", str(source), str(trial_source)], env=env, check=True)
                    if flow == "frontend_build":
                        for path in (source / "frontend/.svelte-kit/output", source / "frontend/build"):
                            shutil.rmtree(path, ignore_errors=True)
                    command = ["/bin/bash", script, "--repo", repo, "--worker", flow, work, trial_source]
                    if leg == "gvisor":
                        command = oci(command, f"bench-{flow.replace('_','-')}-{profile.replace('_','-')}-{run}", profile)
                    suffix = leg if leg == "native" or profile == "defaults" else "gvisor-" + profile
                    log_path = output / f"{flow}-{suffix}-{run}.log"
                    print(f"{flow} {leg} {run}/{args.runs}: {load}", flush=True)
                    with log_path.open("w") as log:
                        log.write("$ " + shlex.join([str(c) for c in command]) + "\n"); log.flush()
                        start = time.perf_counter()
                        code, peak = monitored(command, env, log)
                        row["outer_seconds"] = time.perf_counter() - start
                        row["peak_tree_pss_mib"] = peak
                    markers = [line[len("BENCH_RESULT "):] for line in log_path.read_text().splitlines() if line.startswith("BENCH_RESULT ")]
                    row.update(exit_code=code, log=log_path.name)
                    if code == 0 and len(markers) == 1:
                        row.update(json.loads(markers[0]), status="ok")
                    else:
                        row["status"] = "failed"
                    print(f"  {row['status']}: {row.get('seconds', 'N/A')}", flush=True)
                    shutil.rmtree(work)
                samples.append(row)
                with (output / "samples.jsonl").open("a") as handle:
                    handle.write(json.dumps(row) + "\n")
    table = ["| Flow | Native median (s) | gVisor median (s) | gVisor/native |", "|---|---:|---:|---:|"]
    for flow in flows:
        medians = []
        for leg in ("native", "gvisor"):
            values = [r["seconds"] for r in samples if r["flow"] == flow and r["leg"] == leg and r["profile"] in ("native", "defaults") and r["status"] == "ok"]
            medians.append(statistics.median(values) if len(values) == args.runs else None)
        native, gvisor = medians
        table.append(f"| {flow} | {native:.6f}" if native is not None else f"| {flow} | N/A")
        table[-1] += f" | {gvisor:.6f}" if gvisor is not None else " | N/A"
        table[-1] += f" | {gvisor/native:.2f}x |" if native and gvisor is not None else " | N/A |"
    (output / "summary.md").write_text("\n".join(table) + "\n")
    for row in samples:
        if row["leg"] == "gvisor" and row["status"] == "ok" and row["peak_tree_pss_mib"] is not None:
            info = availability[row["profile"]]
            info["workload_memory_mib"] = max(info["workload_memory_mib"] or 0, row["peak_tree_pss_mib"])
    (output / "configurations.json").write_text(json.dumps(availability, indent=2) + "\n")
finally:
    shutil.rmtree(scratch)
    metadata["scratch_removed"] = not scratch.exists()
    (output / "metadata.json").write_text(json.dumps(metadata, indent=2) + "\n")
sys.exit(1 if any(row["status"] == "failed" for row in samples) else 0)
PY
