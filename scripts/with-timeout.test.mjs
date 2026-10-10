import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { constants, tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { test } from 'node:test';

const helper = fileURLToPath(new URL('./with-timeout.mjs', import.meta.url));

async function run(seconds, source, args = [], interrupt = false) {
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
  const timer = interrupt ? setTimeout(() => child.kill('SIGTERM'), 1000) : undefined;
  child.stderr.on('data', (data) => {
    stderr += data;
  });
  const code = await new Promise((resolve, reject) => {
    child.on('error', reject);
    child.on('exit', resolve);
  });
  clearTimeout(timer);
  return { code, stderr };
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
  test(`cleans descendants that ignore TERM after ${mode}`, { timeout: 15000 }, async () => {
    const scratch = await mkdtemp(join(tmpdir(), 'mainloop-timeout-test-'));
    try {
      const pidFile = join(scratch, 'pid');
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
              : ''
          }
        `;
      const expires = mode.includes('timeout');
      const result = await run(
        expires ? 1 : 5,
        mode.startsWith('nested') ? nested : source,
        [pidFile],
        mode === 'cancellation'
      );
      assert.equal(
        result.code,
        expires ? 124 : mode === 'cancellation' ? 143 : mode === 'nested exit 7' ? 7 : 0
      );
      assert.equal(await readFile(pidFile + '.term', 'utf8'), 'TERM');
      const pid = Number(await readFile(pidFile, 'utf8'));
      // An orphan may briefly remain as a zombie awaiting init's reap; it cannot run.
      let state;
      for (let attempt = 0; attempt < 100; attempt++) {
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
      try {
        const pid = Number(await readFile(join(scratch, 'pid'), 'utf8'));
        try {
          process.kill(pid, 'SIGKILL');
        } catch (error) {
          if (error.code !== 'ESRCH') throw error;
        }
      } catch (error) {
        if (error.code !== 'ENOENT') throw error;
      }
      await rm(scratch, { recursive: true, force: true });
    }
  });
}

for (const signal of ['SIGINT', 'SIGQUIT', 'SIGTERM']) {
  test(`forwards ${signal} unchanged`, { timeout: 10000 }, async () => {
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
        '5',
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
      for (let attempt = 0; attempt < 100; attempt++) {
        try {
          await readFile(ready);
          break;
        } catch (error) {
          if (error.code !== 'ENOENT') throw error;
        }
        await new Promise((resolve) => setTimeout(resolve, 20));
      }
      await readFile(ready);
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
