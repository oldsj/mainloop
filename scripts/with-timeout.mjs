// Supervise a foreground command and its process group without fetching dependencies.
import { spawn } from 'node:child_process';
import { constants } from 'node:os';

const argv = process.argv.slice(2);
// pg_ctl starts a server deliberately; its caller owns that server's cleanup.
const leaveRunning = argv[0] === '--leave-running';
if (leaveRunning) argv.shift();
const [secondsText, label, separator, command, ...args] = argv;
const seconds = Number(secondsText);
if (!Number.isFinite(seconds) || seconds <= 0 || separator !== '--' || !command) {
  console.error('Usage: node with-timeout.mjs SECONDS LABEL -- COMMAND [ARG...]');
  process.exit(2);
}

const started = performance.now();
console.error(`[cap ${seconds} s] ${label}`);
// Nested supervisors finish cancellation before their parent escalates to KILL.
const inheritedGrace = Number(process.env.MAINLOOP_TIMEOUT_GRACE_MS);
const grace =
  Number.isFinite(inheritedGrace) && inheritedGrace > 0 ? Math.min(inheritedGrace, 5000) : 5000;
const child = spawn(command, args, {
  detached: true,
  stdio: 'inherit',
  env: { ...process.env, MAINLOOP_TIMEOUT_GRACE_MS: String(grace / 2) }
});
let result;
let finished = false;
let cleanupTimer;

function signalGroup(signal) {
  if (!child.pid) return;
  try {
    process.kill(-child.pid, signal);
  } catch (error) {
    if (error.code !== 'ESRCH') throw error;
  }
}

function finish() {
  if (finished) return;
  finished = true;
  clearTimeout(deadline);
  clearTimeout(cleanupTimer);
  // Kill descendants even when their parent has already exited.
  if (!leaveRunning || result !== 0) signalGroup('SIGKILL');
  console.error(`[done ${(performance.now() - started) / 1000} s, exit ${result}] ${label}`);
  process.exitCode = result;
}

function stop(code) {
  if (result !== undefined) return;
  result = code;
  clearTimeout(deadline);
  signalGroup('SIGTERM');
  // The leader can exit before a descendant: always finish cleaning the group.
  cleanupTimer = setTimeout(finish, grace);
  try {
    process.kill(-child.pid, 0);
  } catch (error) {
    if (error.code === 'ESRCH') finish();
    else throw error;
  }
}

const deadline = setTimeout(() => {
  console.error(`${label}: timed out after ${seconds} s; terminating its process group`);
  stop(124);
}, seconds * 1000);

for (const name of ['SIGINT', 'SIGQUIT', 'SIGTERM']) {
  process.on(name, () => stop(128 + constants.signals[name]));
}
child.on('error', (error) => {
  console.error(`${label}: ${error.message}`);
  result ??= error.code === 'ENOENT' ? 127 : 126;
  finish();
});
child.on('exit', (code, signal) => {
  if (result !== undefined) return;
  result = code ?? 128 + constants.signals[signal];
  finish();
});
