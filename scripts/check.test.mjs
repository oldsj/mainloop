import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { constants, tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { test } from 'node:test';
import { runChecks } from './check.mjs';

const runner = fileURLToPath(new URL('./check.mjs', import.meta.url));
const cap = fileURLToPath(new URL('./with-timeout.mjs', import.meta.url));
const env = { ...process.env, MAINLOOP_TIMEOUT_GRACE_MS: '400' };
const nodeStep = (name, source, requires = []) => ({
  name,
  seconds: 5,
  command: process.execPath,
  args: ['-e', source],
  requires
});

async function scratchTest(work) {
  const scratch = await mkdtemp(join(tmpdir(), 'mainloop-check-test-'));
  try {
    await work(scratch);
  } finally {
    await rm(scratch, { recursive: true, force: true });
  }
}

test(
  'lanes overlap, steps within each lane wait, and failed checks retain their exit codes',
  { timeout: 10000 },
  async () => {
    await scratchTest(async (scratch) => {
      const left = join(scratch, 'left');
      const right = join(scratch, 'right');
      const ready = (own, other, code) => `
      const fs = require('node:fs');
      fs.writeFileSync(${JSON.stringify(own)}, 'ready');
      const timer = setInterval(() => {
        if (fs.existsSync(${JSON.stringify(other)})) {
          clearInterval(timer); process.exit(${code});
        }
      }, 10);
    `;
      let console = '';
      const exit = await runChecks({
        directory: join(scratch, 'logs'),
        env,
        output: {
          write: (text) => {
            console += text;
          }
        },
        lanes: [
          [
            nodeStep('left', ready(left, right, 7)),
            nodeStep('later', 'console.log("still checked")')
          ],
          [nodeStep('right', ready(right, left, 0))]
        ]
      });
      assert.equal(exit, 7);
      const timing = JSON.parse(await readFile(join(scratch, 'logs', 'timings.json'), 'utf8'));
      assert.deepEqual(
        timing.steps.map((row) => row.exit),
        [7, 0, 0]
      );
      assert.match(console, /\[later\] still checked/);
      assert.match(console, /Check timings/);
      assert.match(console, /left\s+[\d.]+\s+fail \(7\)/);
      assert.ok(timing.steps.every((row) => row.seconds > 0));
    });
  }
);

test(
  'a failed install blocks its consumers but still runs independent lint and the other lane',
  { timeout: 10000 },
  async () => {
    await scratchTest(async (scratch) => {
      const exit = await runChecks({
        directory: scratch,
        env,
        output: { write() {} },
        lanes: [
          [
            nodeStep('install', 'process.exit(9)'),
            nodeStep('build', 'process.exit(0)', ['install']),
            nodeStep('lint', 'process.exit(0)')
          ],
          [nodeStep('backend', 'process.exit(0)')]
        ]
      });
      assert.equal(exit, 9);
      const timing = JSON.parse(await readFile(join(scratch, 'timings.json'), 'utf8'));
      assert.deepEqual(
        timing.steps.map((row) => row.exit),
        [9, null, 0, 0]
      );
      assert.equal(timing.steps[1].status, 'blocked by dependency');
    });
  }
);

test(
  'logs have a byte limit and long or split output always carries its step prefix',
  { timeout: 10000 },
  async () => {
    await scratchTest(async (scratch) => {
      let console = '';
      assert.equal(
        await runChecks({
          directory: scratch,
          env,
          maxLogBytes: 512,
          output: {
            write: (text) => {
              console += text;
            }
          },
          lanes: [
            [
              nodeStep(
                'verbose',
                'process.stdout.write("x".repeat(50000)); process.stderr.write("error\\n");'
              )
            ]
          ]
        }),
        0
      );
      const log = await readFile(join(scratch, 'verbose.log'));
      assert.ok(log.length < 600);
      assert.match(log.toString(), /log truncated/);
      assert.equal((console.match(/x/g) ?? []).length >= 50000, true);
      for (const line of console
        .split('\n')
        .filter((line) => line.includes('xxx') || line.includes('error'))) {
        assert.match(line, /^\[verbose\] /);
      }
    });
  }
);

test(
  'a missing tool or unusable log fails the run and still reports the other steps',
  { timeout: 10000 },
  async () => {
    await scratchTest(async (scratch) => {
      // Opening a directory as a writable log fails even for a root container user.
      await mkdir(join(scratch, 'broken.log'));
      const missing = { name: 'missing', seconds: 5, command: join(scratch, 'missing-tool') };
      const exit = await runChecks({
        directory: scratch,
        env,
        output: { write() {} },
        lanes: [
          [missing, nodeStep('broken', 'process.exit(0)'), nodeStep('other', 'process.exit(0)')]
        ]
      });
      assert.equal(exit, 127);
      const timing = JSON.parse(await readFile(join(scratch, 'timings.json'), 'utf8'));
      assert.deepEqual(
        timing.steps.map((row) => row.exit),
        [127, 1, 0]
      );
    });
  }
);

async function waitFor(path) {
  for (let attempt = 0; attempt < 200; attempt++) {
    try {
      return await readFile(path, 'utf8');
    } catch (error) {
      if (error.code !== 'ENOENT') throw error;
    }
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
  assert.fail(`fixture did not start: ${path}`);
}

async function assertStopped(pid) {
  try {
    const state = (await readFile(`/proc/${pid}/stat`, 'utf8')).split(' ')[2];
    assert.equal(state, 'Z', `descendant ${pid} is still running`);
  } catch (error) {
    if (error.code !== 'ENOENT') throw error;
  }
}

for (const mode of ['deadline', 'SIGINT', 'SIGQUIT', 'SIGTERM']) {
  test(
    `aggregate ${mode} cleans both lanes, prints timing status, and skips pending work`,
    { timeout: 10000 },
    async () => {
      await scratchTest(async (scratch) => {
        const pids = [join(scratch, 'left.pid'), join(scratch, 'right.pid')];
        const termFiles = pids.map((path) => path + '.signal');
        const stubborn = (pidFile, termFile) => `
        const fs = require('node:fs');
        for (const signal of ['SIGINT', 'SIGQUIT', 'SIGTERM']) {
          process.on(signal, () => fs.writeFileSync(${JSON.stringify(termFile)}, signal));
        }
        fs.writeFileSync(${JSON.stringify(pidFile)}, String(process.pid));
        setInterval(() => {}, 1000);
      `;
        const lanes = pids.map((path, index) => [
          nodeStep(index ? 'right' : 'left', stubborn(path, termFiles[index])),
          nodeStep(index ? 'right-later' : 'left-later', 'process.exit(0)')
        ]);
        const source = `
        import { runChecks } from ${JSON.stringify(runner)};
        process.exitCode = await runChecks(${JSON.stringify({ directory: scratch, lanes })});
      `;
        const child = spawn(
          process.execPath,
          [
            cap,
            '--grace',
            '1',
            mode === 'deadline' ? '1' : '8',
            'aggregate',
            '--',
            process.execPath,
            '--input-type=module',
            '-e',
            source
          ],
          { env }
        );
        let output = '';
        child.stdout.on('data', (data) => {
          output += data;
        });
        child.stderr.on('data', (data) => {
          output += data;
        });
        const done = new Promise((resolve, reject) => {
          child.on('error', reject);
          child.on('close', resolve);
        });
        try {
          await Promise.all(pids.map(waitFor));
          if (mode !== 'deadline') child.kill(mode);
          assert.equal(await done, mode === 'deadline' ? 124 : 128 + constants.signals[mode]);
          for (let index = 0; index < pids.length; index++) {
            const pid = Number(await readFile(pids[index], 'utf8'));
            await assertStopped(pid);
            assert.equal(
              await readFile(termFiles[index], 'utf8'),
              mode === 'deadline' ? 'SIGTERM' : mode
            );
          }
          const timing = JSON.parse(await readFile(join(scratch, 'timings.json'), 'utf8'));
          assert.equal(timing.exit, mode === 'deadline' ? 143 : 128 + constants.signals[mode]);
          assert.equal(
            timing.steps.filter((row) => row.status === 'cancelled before start').length,
            2
          );
          assert.match(output, /Check timings/);
        } finally {
          child.kill('SIGTERM');
          await done;
          for (const path of pids) {
            try {
              process.kill(Number(await readFile(path, 'utf8')), 'SIGKILL');
            } catch (error) {
              if (!['ENOENT', 'ESRCH'].includes(error.code)) throw error;
            }
          }
        }
      });
    }
  );
}

test(
  'a step deadline fails the run without abandoning the independent lane',
  { timeout: 10000 },
  async () => {
    await scratchTest(async (scratch) => {
      const timed = nodeStep('timeout', 'setInterval(() => {}, 1000)');
      timed.seconds = 0.2;
      const exit = await runChecks({
        directory: scratch,
        env,
        output: { write() {} },
        lanes: [[timed], [nodeStep('other', 'process.exit(0)')]]
      });
      assert.equal(exit, 124);
      const timing = JSON.parse(await readFile(join(scratch, 'timings.json'), 'utf8'));
      assert.deepEqual(
        timing.steps.map((row) => row.status),
        ['timeout', 'pass']
      );
    });
  }
);

test(
  'the real Make _check recipe cancels and cleans both production lanes',
  { timeout: 10000 },
  async () => {
    await scratchTest(async (scratch) => {
      const bin = join(scratch, 'bin');
      await mkdir(bin);
      const pids = [join(scratch, 'backend.pid'), join(scratch, 'frontend.pid')];
      const source = `#!/usr/bin/env node
      const fs = require('node:fs');
      const path = require('node:path');
      const kind = path.basename(process.argv[1]);
      const backend = kind === 'fixture-make' && process.argv.includes('test-backend');
      const frontend = kind === 'pnpm' && process.argv.includes('check');
      if (!backend && !frontend) process.exit(0);
      const pidFile = backend ? ${JSON.stringify(pids[0])} : ${JSON.stringify(pids[1])};
      process.on('SIGTERM', () => fs.writeFileSync(pidFile + '.term', 'TERM'));
      fs.writeFileSync(pidFile, String(process.pid));
      setInterval(() => {}, 1000);
    `;
      for (const name of ['fixture-make', 'trunk', 'pnpm']) {
        await writeFile(join(bin, name), source, { mode: 0o755 });
      }
      const child = spawn(
        process.execPath,
        [
          cap,
          '8',
          'make fixture',
          '--',
          'make',
          '--no-print-directory',
          '_check',
          `MAKE=${join(bin, 'fixture-make')}`
        ],
        {
          env: { ...env, PATH: `${bin}:${process.env.PATH}`, MAINLOOP_CHECK_LOG_DIR: scratch }
        }
      );
      let output = '';
      child.stdout.on('data', (data) => {
        output += data;
      });
      child.stderr.on('data', (data) => {
        output += data;
      });
      const done = new Promise((resolve, reject) => {
        child.on('error', reject);
        child.on('close', resolve);
      });
      try {
        await Promise.all(pids.map(waitFor));
        child.kill('SIGTERM');
        assert.equal(await done, 143);
        for (const path of pids) {
          await assertStopped(Number(await readFile(path, 'utf8')));
          assert.equal(await readFile(path + '.term', 'utf8'), 'TERM');
        }
        const timing = JSON.parse(await readFile(join(scratch, 'timings.json'), 'utf8'));
        assert.equal(
          timing.steps.find((row) => row.step === 'frontend-build').status,
          'cancelled before start'
        );
        assert.match(output, /Check timings/);
      } finally {
        child.kill('SIGTERM');
        await done;
        for (const path of pids) {
          try {
            process.kill(Number(await readFile(path, 'utf8')), 'SIGKILL');
          } catch (error) {
            if (!['ENOENT', 'ESRCH'].includes(error.code)) throw error;
          }
        }
      }
    });
  }
);
