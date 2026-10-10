// Explicit offline regressions through the real Make runner and dev-image PG16.
import assert from 'node:assert/strict';
import { spawn, spawnSync } from 'node:child_process';
import { copyFile, mkdtemp, readFile, rm, stat, writeFile } from 'node:fs/promises';
import { constants, tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { test } from 'node:test';

const root = fileURLToPath(new URL('../', import.meta.url));
const helper = join(root, 'scripts/with-timeout.mjs');
const delay = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

async function ready(path) {
  for (let attempt = 0; attempt < 250; attempt++) {
    try {
      return await readFile(path, 'utf8');
    } catch (error) {
      if (error.code !== 'ENOENT') throw error;
    }
    await delay(20);
  }
  throw new Error(`Fixture did not become ready: ${path}`);
}

async function assertStopped(pid) {
  try {
    const state = (await readFile(`/proc/${pid}/stat`, 'utf8')).split(' ')[2];
    assert.equal(state, 'Z', `process ${pid} remains runnable (${state})`);
  } catch (error) {
    if (error.code !== 'ENOENT') throw error;
  }
}

function killGroup(pid) {
  try {
    process.kill(-pid, 'SIGKILL');
  } catch (error) {
    if (error.code !== 'ESRCH') throw error;
  }
}

function start(command, args, options = {}) {
  const child = spawn(command, args, { stdio: ['ignore', 'ignore', 'pipe'], ...options });
  let stderr = '';
  child.stderr.on('data', (data) => {
    stderr += data;
  });
  const done = new Promise((resolve, reject) => {
    child.on('error', reject);
    child.on('close', (code, signal) => resolve({ code, signal, stderr }));
  });
  return { child, done };
}

async function postgresWorker() {
  const pg = '/usr/lib/postgresql/16/bin/pg_ctl';
  await copyFile(pg, pg + '.real');
  // Seven seconds fits the 15-second stop phase but exceeds the old outer grace.
  await writeFile(
    pg,
    `#!/bin/bash
for arg in "$@"; do
  if [[ "$arg" == stop ]]; then sleep 7; fi
  if [[ "$arg" == start && \${TIMEOUT_SLOW_START:-0} == 1 ]]; then
    trap '' TERM
    /usr/lib/postgresql/16/bin/pg_ctl.real "$@" || exit "$?"
    printf '%s' "$$" > "\${DEV_POSTGRES_ROOT}/startup.pid"
    sleep 20
    exit 0
  fi
done
exec /usr/lib/postgresql/16/bin/pg_ctl.real "$@"
`,
    { mode: 0o755 }
  );
  const deniedRoot = '/data/timeout-insufficient-budget';
  const denied = await start(
    process.execPath,
    [
      helper,
      '--grace',
      '3',
      '10',
      'short enclosing budget',
      '--',
      '/usr/local/bin/dev-postgres',
      'run',
      'true'
    ],
    { env: { ...process.env, DEV_POSTGRES_ROOT: deniedRoot } }
  ).done;
  assert.equal(denied.code, 2, denied.stderr);
  assert.match(denied.stderr, /enclosing cleanup budget must allow 25 seconds/);
  await assert.rejects(stat(deniedRoot), { code: 'ENOENT' });
  for (const mode of ['SIGTERM', 'deadline', 'startup']) {
    const pgdata = `/data/timeout-${mode}`;
    const pidFile = mode === 'startup' ? join(pgdata, 'startup.pid') : pgdata + '.command';
    let commandPid;
    let serverPid;
    const source = `
      require('node:fs').writeFileSync(process.argv[1], String(process.pid));
      process.on('SIGTERM', () => {});
      setInterval(() => {}, 1000);
    `;
    const args = ['/usr/local/bin/dev-postgres', 'run', process.execPath, '-e', source, pidFile];
    const env = {
      ...process.env,
      DEV_POSTGRES_ROOT: pgdata,
      DEV_POSTGRES_PORT: '55432',
      TIMEOUT_SLOW_START: mode === 'startup' ? '1' : '0'
    };
    const { child, done } =
      mode === 'deadline'
        ? start(process.execPath, [helper, '2', 'enclosing PostgreSQL cap', '--', ...args], {
            env
          })
        : start(args[0], args.slice(1), {
            env
          });
    try {
      commandPid = Number(await ready(pidFile));
      serverPid = Number((await readFile(join(pgdata, 'postmaster.pid'), 'utf8')).split('\n')[0]);
      const interrupted = performance.now();
      if (mode !== 'deadline') child.kill('SIGTERM');
      const result = await done;
      assert.equal(result.code, mode === 'deadline' ? 124 : 143, result.stderr);
      assert.ok(
        performance.now() - interrupted >= 7000,
        'slow stop must finish before the wrapper exits'
      );
      await assertStopped(commandPid);
      await assertStopped(serverPid);
      for (const path of [pgdata, pgdata + '-socket', pgdata + '-tmp']) {
        await assert.rejects(stat(path), { code: 'ENOENT' }, `owned directory remains: ${path}`);
      }
      assert.match(result.stderr, /exit 0\] dev-postgres stop/);
      console.error(
        `${mode}: command/server stopped and all owned directories removed\n${result.stderr}`
      );
    } finally {
      child.kill('SIGTERM');
      await done;
      for (const pid of [commandPid, serverPid]) {
        if (pid) {
          try {
            process.kill(pid, 'SIGKILL');
          } catch (error) {
            if (error.code !== 'ESRCH') throw error;
          }
        }
      }
      for (const path of [pgdata, pgdata + '-socket', pgdata + '-tmp', pidFile]) {
        await rm(path, { recursive: true, force: true });
      }
    }
  }
}

if (process.argv[2] === '--postgres-worker') {
  await postgresWorker();
} else {
  for (const mode of ['deadline', 'short budget', 'SIGTERM', 'SIGINT', 'SIGQUIT']) {
    test(
      `real make test-backend cleans TERM-resistant worker after ${mode}`,
      { timeout: 45000 },
      async () => {
        const scratch = await mkdtemp(join(tmpdir(), 'mainloop-runner-cleanup-'));
        const pidFile = join(scratch, 'worker');
        let worker;
        let descendant;
        const fixture = `import os, signal, subprocess, sys, time, unittest
from pathlib import Path
class StubbornWorker(unittest.TestCase):
 def test_wait(self):
  for signum in (signal.SIGTERM, signal.SIGINT, signal.SIGQUIT): signal.signal(signum, signal.SIG_IGN)
  child = subprocess.Popen([sys.executable, '-c', "import os, signal, time; from pathlib import Path; [signal.signal(s, signal.SIG_IGN) for s in (signal.SIGTERM, signal.SIGINT, signal.SIGQUIT)]; Path(os.environ['TIMEOUT_REGRESSION_PID']+'.child').write_text(str(os.getpid())); time.sleep(60)"])
  Path(os.environ['TIMEOUT_REGRESSION_PID']).write_text(str(os.getpid()))
  time.sleep(60)
`;
        await writeFile(join(scratch, 'timeout_regression.py'), fixture);
        const expires = mode === 'deadline' || mode === 'short budget';
        const { child, done } = start(
          process.execPath,
          [
            helper,
            '--grace',
            mode === 'short budget' ? '3' : '30',
            expires ? '2' : '30',
            'enclosing backend cap',
            '--',
            'make',
            'test-backend',
            'TEST_ARGS=timeout_regression'
          ],
          {
            cwd: root,
            env: {
              ...process.env,
              PYTHONPATH: scratch,
              TIMEOUT_REGRESSION_PID: pidFile,
              MAINLOOP_TEST_DATABASE_URL: 'postgresql://fixture-unused/postgres'
            }
          }
        );
        try {
          worker = Number(await ready(pidFile));
          descendant = Number(await ready(pidFile + '.child'));
          if (!expires) child.kill(mode);
          const result = await done;
          assert.equal(result.code, expires ? 124 : 128 + constants.signals[mode], result.stderr);
          await assertStopped(worker);
          await assertStopped(descendant);
        } finally {
          child.kill('SIGTERM');
          await done;
          if (worker) killGroup(worker);
          await rm(scratch, { recursive: true, force: true });
        }
      }
    );
  }

  test(
    'dev-postgres completes a slow owned stop before cancellation or enclosing expiry returns',
    { timeout: 100000 },
    async () => {
      const image = process.env.MAINLOOP_TIMEOUT_TEST_IMAGE;
      assert.ok(
        image,
        'Set MAINLOOP_TIMEOUT_TEST_IMAGE to an existing local development image with PG16; this regression never pulls an image'
      );
      const inspected = spawnSync('docker', ['image', 'inspect', image], {
        timeout: 10000,
        stdio: 'ignore'
      });
      assert.equal(inspected.status, 0, `local image unavailable: ${image}`);
      const name = `mainloop-timeout-regression-${process.pid}`;
      try {
        const { done } = start('docker', [
          'run',
          '--name',
          name,
          '--label',
          'mainloop.task=timeouts-repair-1',
          '--user',
          '0',
          '--network',
          'none',
          '--tmpfs',
          '/data:rw,size=512m',
          '--volume',
          `${root}:/workspace:ro`,
          '--volume',
          `${root}/scripts/with-timeout.mjs:/usr/local/lib/mainloop/with-timeout.mjs:ro`,
          '--volume',
          `${root}/.devcontainer/dev-postgres:/usr/local/bin/dev-postgres:ro`,
          '--entrypoint',
          'node',
          image,
          '/workspace/scripts/with-timeout.integration.test.mjs',
          '--postgres-worker'
        ]);
        const result = await done;
        assert.equal(result.code, 0, result.stderr);
        console.error(result.stderr);
      } finally {
        const removed = spawnSync('docker', ['rm', '-f', name], {
          timeout: 15000,
          encoding: 'utf8'
        });
        assert.equal(removed.status, 0, removed.stderr);
      }
    }
  );
}
