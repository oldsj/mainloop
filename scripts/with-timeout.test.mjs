import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { test } from 'node:test';

const helper = fileURLToPath(new URL('./with-timeout.mjs', import.meta.url));

async function run(seconds, source, args = [], interrupt = false) {
  const child = spawn(process.execPath, [
    helper,
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

for (const mode of ['timeout', 'normal exit', 'cancellation', 'nested timeout']) {
  test(`cleans descendants that ignore TERM after ${mode}`, { timeout: 15000 }, async () => {
    const scratch = await mkdtemp(join(tmpdir(), 'mainloop-timeout-test-'));
    try {
      const pidFile = join(scratch, 'pid');
      const source = `
        const { spawn } = require('node:child_process');
        const { writeFileSync } = require('node:fs');
        const child = spawn(process.execPath, ['-e', "process.on('SIGTERM', () => {}); setInterval(() => {}, 1000)"], { stdio: 'ignore' });
        writeFileSync(process.argv[1], String(child.pid));
        child.unref();
        ${mode === 'normal exit' ? 'setTimeout(() => process.exit(0), 100);' : 'setInterval(() => {}, 1000);'}
      `;
      const nested = `
          require('node:child_process').spawn(process.execPath,
            [${JSON.stringify(helper)}, '60', 'nested', '--', process.execPath, '-e', ${JSON.stringify(source)}, process.argv[1]],
            { stdio: 'inherit' });
        `;
      const expires = mode.includes('timeout');
      const result = await run(
        expires ? 1 : 5,
        mode === 'nested timeout' ? nested : source,
        [pidFile],
        mode === 'cancellation'
      );
      assert.equal(result.code, expires ? 124 : mode === 'cancellation' ? 143 : 0);
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
      await rm(scratch, { recursive: true, force: true });
    }
  });
}
