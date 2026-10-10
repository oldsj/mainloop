// Supervise a foreground command and its process group without fetching dependencies.
import { spawn } from 'node:child_process';
import { constants } from 'node:os';

const argv = process.argv.slice(2);
let requestedGrace = 30;
if (argv[0] === '--grace') {
  requestedGrace = Number(argv[1]);
  argv.splice(0, 2);
}
// pg_ctl starts a server deliberately; its caller owns that server's cleanup.
const leaveRunning = argv[0] === '--leave-running';
if (leaveRunning) argv.shift();
const [secondsText, label, separator, command, ...args] = argv;
const seconds = Number(secondsText);
if (
  !Number.isFinite(seconds) ||
  seconds <= 0 ||
  !Number.isFinite(requestedGrace) ||
  requestedGrace <= 0 ||
  requestedGrace > 30 ||
  separator !== '--' ||
  !command
) {
  console.error(
    'Usage: node with-timeout.mjs [--grace SECONDS (0 < seconds <= 30)] [--leave-running] SECONDS LABEL -- COMMAND [ARG...]'
  );
  process.exit(2);
}

const started = performance.now();
console.error(`[cap ${seconds} s] ${label}`);
// Each owner reserves time after its children escalate. Non-Node supervisors
// must use the same inherited budget; the backend runner does so explicitly.
const inheritedGrace = Number(process.env.MAINLOOP_TIMEOUT_GRACE_MS);
const grace =
  Number.isFinite(inheritedGrace) && inheritedGrace > 0
    ? Math.min(requestedGrace * 1000, inheritedGrace - Math.min(1000, inheritedGrace / 4))
    : requestedGrace * 1000;
const child = spawn(command, args, {
  detached: true,
  stdio: 'inherit',
  env: { ...process.env, MAINLOOP_TIMEOUT_GRACE_MS: String(grace) }
});
let result;
let finished = false;
let cleanupTimer;
let cleanupPoll;

function signalGroup(signal) {
  if (!child.pid) return false;
  try {
    process.kill(-child.pid, signal);
    return true;
  } catch (error) {
    if (error.code !== 'ESRCH') throw error;
    return false;
  }
}

function finish() {
  if (finished) return;
  finished = true;
  clearTimeout(deadline);
  clearTimeout(cleanupTimer);
  clearInterval(cleanupPoll);
  // Escalate only after giving every remaining descendant its cleanup window.
  if (!leaveRunning || result !== 0) signalGroup('SIGKILL');
  console.error(`[done ${(performance.now() - started) / 1000} s, exit ${result}] ${label}`);
  process.exitCode = result;
}

function pollCleanup() {
  if (!signalGroup(0)) finish();
}

function stop(code, signal = 'SIGTERM') {
  if (result !== undefined) return;
  result = code;
  clearTimeout(deadline);
  signalGroup(signal);
  // The leader can exit before a descendant: always finish cleaning the group.
  cleanupTimer = setTimeout(finish, grace);
  cleanupPoll = setInterval(pollCleanup, 25);
  pollCleanup();
}

const deadline = setTimeout(() => {
  console.error(`${label}: timed out after ${seconds} s; terminating its process group`);
  stop(124);
}, seconds * 1000);

for (const name of ['SIGINT', 'SIGQUIT', 'SIGTERM']) {
  process.on(name, () => stop(128 + constants.signals[name], name));
}
child.on('error', (error) => {
  console.error(`${label}: ${error.message}`);
  result ??= error.code === 'ENOENT' ? 127 : 126;
  finish();
});
child.on('exit', (code, signal) => {
  if (result !== undefined) {
    pollCleanup();
    return;
  }
  const codeToKeep = code ?? 128 + constants.signals[signal];
  if (leaveRunning && codeToKeep === 0) {
    result = 0;
    finish();
  } else {
    // A successful leader can leave supervisors with separate child groups.
    // Notify those owners before killing this group, and keep the leader's code.
    stop(codeToKeep);
  }
});
