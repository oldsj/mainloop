// Run two bounded lanes, with formatting complete before tests read the source.
import { spawn } from 'node:child_process';
import { closeSync, mkdirSync, openSync, writeFileSync, writeSync } from 'node:fs';
import { constants } from 'node:os';
import { join } from 'node:path';
import { StringDecoder } from 'node:string_decoder';
import { fileURLToPath } from 'node:url';

const helper = fileURLToPath(new URL('./with-timeout.mjs', import.meta.url));
const logLimit = 8 * 1024 * 1024;

export function checkPlan(make = 'make', nonBackend = false) {
  const step = (name, seconds, command, args = [], requires = []) => ({
    name,
    seconds,
    command,
    args,
    requires
  });
  return {
    // Only this phase can change source. The standalone `make fmt` still also fixes lint.
    before: [step('fmt', 180, 'trunk', ['fmt', '--upstream', 'origin/main'])],
    lanes: [
      ...(nonBackend
        ? []
        : [[step('backend', 550, make, ['--no-print-directory', '-j1', 'test-backend'])]]),
      [
        step('test-timeouts', 30, make, ['--no-print-directory', '-j1', 'test-timeouts']),
        step('install-frontend', 120, make, ['--no-print-directory', '-j1', 'install-frontend']),
        step('check-frontend', 120, 'pnpm', ['check'], ['install-frontend']),
        step('test-frontend', 60, 'pnpm', ['--dir', 'frontend', 'test:unit'], ['install-frontend']),
        step('frontend-build', 120, 'pnpm', ['--dir', 'frontend', 'build'], ['install-frontend']),
        // One complete lint pass, without writes racing either test lane.
        step('lint', 120, 'trunk', ['check', '--upstream', 'origin/main', '--no-fix'])
      ]
    ]
  };
}

export async function runChecks({
  before = [],
  lanes,
  directory,
  env = process.env,
  output = process.stdout,
  maxLogBytes = logLimit
}) {
  mkdirSync(directory, { recursive: true });
  const steps = [...before, ...lanes.flat()];
  const results = new Map(
    steps.map((step) => [step.name, { step: step.name, seconds: 0, status: 'not run', exit: null }])
  );
  const active = new Set();
  let cancelled;
  const started = performance.now();
  const handlers = new Map(
    ['SIGINT', 'SIGQUIT', 'SIGTERM'].map((signal) => [
      signal,
      () => {
        cancelled ??= 128 + constants.signals[signal];
        // Each cap owns a separate command group and waits for its descendants to stop.
        for (const child of active) child.kill(signal);
      }
    ])
  );
  for (const [signal, handler] of handlers) process.on(signal, handler);

  async function run(step) {
    const result = results.get(step.name);
    if (cancelled || step.requires?.some((name) => results.get(name)?.exit !== 0)) {
      result.status = cancelled ? 'cancelled before start' : 'blocked by dependency';
      return;
    }
    const stepStarted = performance.now();
    let log;
    let logError;
    let child;
    let logged = 0;
    let truncated = false;
    const capture = (chunk) => {
      const length = Math.min(chunk.length, Math.max(0, maxLogBytes - logged));
      if (length) writeSync(log, chunk.subarray(0, length));
      logged += length;
      if (length < chunk.length && !truncated) {
        truncated = true;
        writeSync(log, '\n[log truncated; command output continues on the console]\n');
      }
    };
    const prefix = (stream) => {
      const decoder = new StringDecoder('utf8');
      let pending = '';
      const flush = (text) => {
        pending += text.replace(/\r/g, '\n');
        let end;
        // Bound partial lines too: a tool can emit arbitrary output without a newline.
        while ((end = pending.indexOf('\n')) >= 0 || pending.length >= 16384) {
          const size = end >= 0 ? Math.min(end, 16384) : 16384;
          output.write(`[${step.name}] ${pending.slice(0, size)}\n`);
          pending = pending.slice(size + (size === end ? 1 : 0));
        }
      };
      stream.on('data', (chunk) => {
        try {
          if (!logError) capture(chunk);
        } catch (error) {
          logError = error;
          output.write(`[${step.name}] cannot write log: ${error.message}\n`);
          child.kill('SIGTERM');
        }
        flush(decoder.write(chunk));
        if (output.writableNeedDrain) {
          stream.pause();
          output.once('drain', () => stream.resume());
        }
      });
      stream.on('end', () => {
        flush(decoder.end());
        if (pending) output.write(`[${step.name}] ${pending}\n`);
      });
    };
    output.write(
      `[${step.name}] starting (${step.seconds}s cap); log: ${join(directory, `${step.name}.log`)}\n`
    );
    try {
      log = openSync(join(directory, `${step.name}.log`), 'w');
      child = spawn(
        process.execPath,
        [helper, String(step.seconds), step.name, '--', step.command, ...(step.args ?? [])],
        {
          env,
          stdio: ['ignore', 'pipe', 'pipe']
        }
      );
      active.add(child);
      prefix(child.stdout);
      prefix(child.stderr);
      result.exit = await new Promise((resolve) => {
        child.on('error', (error) => {
          output.write(`[${step.name}] ${error.message}\n`);
          resolve(error.code === 'ENOENT' ? 127 : 126);
        });
        child.on('close', (code, signal) => resolve(code ?? 128 + constants.signals[signal]));
      });
      active.delete(child);
      if (logError) result.exit = 1;
    } catch (error) {
      output.write(`[${step.name}] ${error.message}\n`);
      result.exit = 1;
    } finally {
      result.status =
        result.exit === 0 ? 'pass' : result.exit === 124 ? 'timeout' : `fail (${result.exit})`;
      if (log !== undefined) closeSync(log);
      result.seconds = (performance.now() - stepStarted) / 1000;
    }
  }

  try {
    for (const step of before) await run(step);
    await Promise.all(
      lanes.map(async (lane) => {
        for (const step of lane) await run(step);
      })
    );
  } finally {
    for (const [signal, handler] of handlers) process.off(signal, handler);
    const rows = [...results.values()];
    const seconds = (performance.now() - started) / 1000;
    const exit = cancelled ?? rows.find((row) => row.exit !== null && row.exit !== 0)?.exit ?? 0;
    writeFileSync(
      join(directory, 'timings.json'),
      JSON.stringify({ seconds, exit, steps: rows }, null, 2) + '\n'
    );
    output.write('\nCheck timings (includes step cleanup):\n');
    output.write('Step                  Seconds  Status\n');
    for (const row of rows)
      output.write(`${row.step.padEnd(21)} ${row.seconds.toFixed(2).padStart(7)}  ${row.status}\n`);
    output.write(`${'total wall'.padEnd(21)} ${seconds.toFixed(2).padStart(7)}  exit ${exit}\n`);
  }
  return (
    cancelled ?? [...results.values()].find((row) => row.exit !== null && row.exit !== 0)?.exit ?? 0
  );
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const args = process.argv.slice(2);
  let make = 'make';
  if (args[0] === '--make') {
    make = args[1];
    args.splice(0, 2);
  }
  if (!make || args.length > 1 || (args.length === 1 && args[0] !== '--non-backend')) {
    console.error('Usage: node scripts/check.mjs [--make MAKE] [--non-backend]');
    process.exitCode = 2;
  } else {
    process.exitCode = await runChecks({
      ...checkPlan(make, args[0] === '--non-backend'),
      directory: process.env.MAINLOOP_CHECK_LOG_DIR || '.cache/check'
    });
  }
}
