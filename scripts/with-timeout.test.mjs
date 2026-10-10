import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { access, mkdtemp, readFile, rm } from 'node:fs/promises';
import { constants, tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { test } from 'node:test';

const helper = fileURLToPath(new URL('./with-timeout.mjs', import.meta.url));

// Under CPU contention, process startup can take seconds. Poll on a wall-clock
// bound instead of a fixed attempt count.
async function exists(path, seconds = 10) {
  const expires = performance.now() + seconds * 1000;
  for (;;) {
    try {
      await access(path);
      return true;
    } catch (error) {
      if (error.code !== 'ENOENT') throw error;
    }
    if (performance.now() > expires) return false;
    await new Promise((resolve) => setTimeout(resolve, 20));
  }
}

// interruptWhen: send SIGTERM once this file exists, not after a fixed delay.
async function run(seconds, source, args = [], interruptWhen) {
  const child = spawn(process.execPath, [
    helper,
    '--grace',
    '0.5',
    String(seconds),
    'fixture',
    '--',
    process.execPath,
    '-e',
    source,
    ...args
  ]);
  let stderr = '';
  child.stderr.on('data', (data) => {
    stderr += data;
  });
  const exited = new Promise((resolve, reject) => {
    child.on('error', reject);
    child.on('exit', resolve);
  });
  if (interruptWhen && (await exists(interruptWhen))) child.kill('SIGTERM');
  return { code: await exited, stderr };
}

test(
  'deadline terminates a hung command with exit 124 and a clear message',
  { timeout: 10000 },
  async () => {
    const result = await run(0.2, 'setInterval(() => {}, 1000)');
    assert.equal(result.code, 124);
    assert.match(result.stderr, /fixture: timed out after 0.2 s/);
  }
);

test('a nested cap clamps to just before its enclosing deadline', async () => {
  const report = `process.stdout.write(JSON.stringify({
    deadline: Number(process.env.MAINLOOP_TIMEOUT_DEADLINE_MS),
    grace: Number(process.env.MAINLOOP_TIMEOUT_GRACE_MS)
  }))`;
  const capture = (env, seconds) =>
    new Promise((resolve, reject) => {
      const child = spawn(
        process.execPath,
        [helper, String(seconds), 'inner', '--', process.execPath, '-e', report],
        { env: { ...process.env, ...env } }
      );
      let stdout = '';
      let stderr = '';
      child.stdout.on('data', (data) => (stdout += data));
      child.stderr.on('data', (data) => (stderr += data));
      child.on('error', reject);
      child.on('exit', (code) => resolve({ code, stderr, stdout }));
    });
  const parent = Date.now() + 100000;
  const clamped = await capture(
    { MAINLOOP_TIMEOUT_DEADLINE_MS: String(parent), MAINLOOP_TIMEOUT_GRACE_MS: '8000' },
    500
  );
  assert.equal(clamped.code, 0);
  assert.match(
    clamped.stderr,
    /\[cap 9\d\.\d+ s \(500 s, clamped to the enclosing deadline\)\] inner/
  );
  const exported = JSON.parse(clamped.stdout);
  // Deadline and grace both leave the parent a one-second reserve.
  assert.ok(exported.deadline <= parent - 1000);
  assert.ok(exported.deadline > parent - 1000 - 5000);
  assert.equal(exported.grace, 7000);
  const unclamped = await capture(
    { MAINLOOP_TIMEOUT_DEADLINE_MS: String(parent), MAINLOOP_TIMEOUT_GRACE_MS: '8000' },
    5
  );
  assert.match(unclamped.stderr, /\[cap 5 s\] inner/);
  const spent = await capture(
    { MAINLOOP_TIMEOUT_DEADLINE_MS: String(Date.now() + 500), MAINLOOP_TIMEOUT_GRACE_MS: '8000' },
    5
  );
  assert.equal(spent.code, 124);
  assert.equal(spent.stdout, '');
  assert.match(spent.stderr, /inner: timed out after 0 s; the enclosing deadline left no time/);
});

test('ordinary exit codes pass through', { timeout: 10000 }, async () => {
  for (const code of [0, 7, 137]) {
    assert.equal((await run(5, `process.exit(${code})`)).code, code);
  }
});

for (const mode of [
  'timeout',
  'normal exit',
  'cancellation',
  'nested timeout',
  'nested exit 0',
  'nested exit 7'
]) {
  test(`cleans descendants that ignore TERM after ${mode}`, { timeout: 60000 }, async () => {
    const scratch = await mkdtemp(join(tmpdir(), 'mainloop-timeout-test-'));
    try {
      const stubborn = `
        const fs = require('node:fs');
        process.on('SIGTERM', () => fs.writeFileSync(process.argv[1] + '.term', 'TERM'));
        fs.writeFileSync(process.argv[1], String(process.pid));
        setInterval(() => {}, 1000);
      `;
      const source = `
        const { spawn } = require('node:child_process');
        const child = spawn(process.execPath, ['-e', ${JSON.stringify(stubborn)}, process.argv[1]], { stdio: 'ignore' });
        child.unref();
        ${mode === 'normal exit' ? "setInterval(() => { if (require('node:fs').existsSync(process.argv[1])) process.exit(0); }, 10);" : 'setInterval(() => {}, 1000);'}
      `;
      const nested = `
          const inner = require('node:child_process').spawn(process.execPath,
            [${JSON.stringify(helper)}, '60', 'nested', '--', process.execPath, '-e', ${JSON.stringify(source)}, process.argv[1]],
            { stdio: 'inherit' });
          ${
            mode.startsWith('nested exit')
              ? `inner.unref(); const ready = setInterval(() => {
            if (require('node:fs').existsSync(process.argv[1])) process.exit(${mode.endsWith('7') ? 7 : 0});
          }, 10);`
              : // Like Make, pass on the inner cap's status.
                "inner.on('exit', (code, signal) => process.exit(code ?? 128));"
          }
        `;
      const expires = mode.includes('timeout');
      // A deadline proves cleanup only if the descendant started before it. When a
      // loaded host starts the fixture too late, retry with a longer deadline.
      let pidFile;
      let result;
      for (const seconds of expires ? [1, 2, 4, 8] : [20]) {
        pidFile = join(scratch, `pid-${seconds}`);
        result = await run(
          seconds,
          mode.startsWith('nested') ? nested : source,
          [pidFile],
          mode === 'cancellation' ? pidFile : undefined
        );
        if (await exists(pidFile, 0)) break;
      }
      assert.equal(
        result.code,
        expires ? 124 : mode === 'cancellation' ? 143 : mode === 'nested exit 7' ? 7 : 0
      );
      if (mode === 'nested timeout') {
        // The inner cap expires just before the outer one and reports the timeout.
        const inner = result.stderr.indexOf('nested: timed out after');
        assert.ok(inner >= 0, result.stderr);
        const outer = result.stderr.indexOf('fixture: timed out after');
        assert.ok(outer < 0 || inner < outer, result.stderr);
      }
      assert.equal(await readFile(pidFile + '.term', 'utf8'), 'TERM');
      const pid = Number(await readFile(pidFile, 'utf8'));
      // An orphan may briefly remain as a zombie awaiting init's reap; it cannot run.
      let state;
      const reaped = performance.now() + 10000;
      while (performance.now() < reaped) {
        try {
          state = (await readFile(`/proc/${pid}/stat`, 'utf8')).split(' ')[2];
        } catch (error) {
          if (error.code === 'ENOENT') return;
          throw error;
        }
        if (state === 'Z') return;
        await new Promise((resolve) => setTimeout(resolve, 20));
      }
      assert.fail(`descendant ${pid} still running (${state})`);
    } finally {
      for (const seconds of [1, 2, 4, 8, 20]) {
        try {
          const pid = Number(await readFile(join(scratch, `pid-${seconds}`), 'utf8'));
          try {
            process.kill(pid, 'SIGKILL');
          } catch (error) {
            if (error.code !== 'ESRCH') throw error;
          }
        } catch (error) {
          if (error.code !== 'ENOENT') throw error;
        }
      }
      await rm(scratch, { recursive: true, force: true });
    }
  });
}

for (const signal of ['SIGINT', 'SIGQUIT', 'SIGTERM']) {
  test(`forwards ${signal} unchanged`, { timeout: 30000 }, async () => {
    const scratch = await mkdtemp(join(tmpdir(), 'mainloop-signal-test-'));
    const ready = join(scratch, 'ready');
    const received = join(scratch, 'signal');
    const source = `
      const fs = require('node:fs');
      for (const name of ['SIGINT', 'SIGQUIT', 'SIGTERM']) process.on(name, () => {
        fs.writeFileSync(process.argv[2], name); process.exit(0);
      });
      fs.writeFileSync(process.argv[1], 'ready');
      setInterval(() => {}, 1000);
    `;
    const child = spawn(
      process.execPath,
      [
        helper,
        '--grace',
        '0.5',
        '20',
        'signals',
        '--',
        process.execPath,
        '-e',
        source,
        ready,
        received
      ],
      { stdio: 'ignore' }
    );
    const done = new Promise((resolve, reject) => {
      child.on('error', reject);
      child.on('exit', resolve);
    });
    try {
      assert.ok(await exists(ready, 15), 'signal fixture did not start');
      child.kill(signal);
      assert.equal(await done, 128 + constants.signals[signal]);
      assert.equal(await readFile(received, 'utf8'), signal);
    } finally {
      child.kill('SIGTERM');
      await done;
      await rm(scratch, { recursive: true, force: true });
    }
  });
}
