'use strict';

import assert from 'node:assert/strict';
import fs from 'node:fs';
import http from 'node:http';
import os from 'node:os';
import path from 'node:path';
import { spawn } from 'node:child_process';
import { once } from 'node:events';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const shim = path.resolve(__dirname, '../live-agent-image/exec-shim.js');

async function startShim(home, fakeBin, shimPath, extraEnv = {}) {
  const child = spawn(process.execPath, [shimPath], {
    env: {
      ...process.env,
      ...extraEnv,
      HOME: home,
      PATH: `${fakeBin}:${process.env.PATH}`,
      HERDR_SESSION: 'shim-test',
      EXEC_SHIM_PANE_ID: 'pane-test',
      EXEC_SHIM_PORT: '0',
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  let output = '';
  const port = await new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error(`shim did not start: ${output}`)), 5000);
    child.once('error', reject);
    child.once('exit', (code) => reject(new Error(`shim exited ${code}: ${output}`)));
    child.stdout.on('data', (chunk) => {
      output += chunk;
      const match = output.match(/exec-shim listening on :(\d+)/);
      if (match) {
        clearTimeout(timer);
        resolve(Number(match[1]));
      }
    });
    child.stderr.on('data', (chunk) => {
      output += chunk;
    });
  });
  return { child, port, output: () => output };
}

function request(port, method, route, { body, token } = {}) {
  return new Promise((resolve, reject) => {
    const headers = {};
    if (body !== undefined) headers['content-type'] = 'application/json';
    if (token !== undefined) headers.authorization = `Bearer ${token}`;
    const req = http.request(
      { host: '127.0.0.1', port, method, path: route, headers },
      (res) => {
        const chunks = [];
        res.on('data', (chunk) => chunks.push(chunk));
        res.on('end', () =>
          resolve({ status: res.statusCode, body: Buffer.concat(chunks).toString('utf8') }),
        );
      },
    );
    req.once('error', reject);
    if (body !== undefined) req.end(JSON.stringify(body));
    else req.end();
  });
}

async function stop(child) {
  if (child.exitCode !== null || child.signalCode !== null) return;
  child.kill('SIGTERM');
  await once(child, 'exit');
}

test('shim token gates run/read, is one-time, private, and survives process restart', async (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'exec-shim-test-'));
  const home = path.join(root, 'home');
  const fakeBin = path.join(root, 'bin');
  const shimPath = path.join(root, 'exec-shim.js');
  fs.mkdirSync(home);
  fs.mkdirSync(fakeBin);
  fs.copyFileSync(shim, shimPath);
  const herdr = path.join(fakeBin, 'herdr');
  fs.writeFileSync(
    herdr,
    '#!/bin/sh\nif [ "$3" = "status" ] && [ "$4" = "server" ]; then echo "status: running"; exit 0; fi\nif [ "$3" = "pane" ] && [ "$4" = "read" ]; then echo "fixture pane"; exit 0; fi\nexit 0\n',
    { mode: 0o700 },
  );

  let running = await startShim(home, fakeBin, shimPath);
  t.after(async () => {
    await stop(running.child);
    fs.rmSync(root, { recursive: true, force: true });
  });

  assert.equal((await request(running.port, 'GET', '/healthz')).status, 200);
  assert.equal((await request(running.port, 'GET', '/read')).status, 200);
  assert.equal(
    (await request(running.port, 'POST', '/run', { body: { command: 'echo fixture' } })).status,
    200,
  );

  const token = 'fixture-only-token-with-at-least-thirty-two-characters';
  assert.equal(
    (await request(running.port, 'POST', '/token', { body: { token } })).status,
    201,
  );
  const tokenPath = path.join(home, '.mainloop', 'exec-shim-token');
  assert.equal(fs.statSync(tokenPath).mode & 0o777, 0o600);
  assert.equal(fs.statSync(path.dirname(tokenPath)).mode & 0o777, 0o700);
  assert.equal(fs.readFileSync(tokenPath, 'utf8'), token);
  assert.equal((await request(running.port, 'GET', '/healthz')).status, 200);
  assert.equal((await request(running.port, 'GET', '/read')).status, 401);
  assert.equal((await request(running.port, 'GET', '/read', { token: `${token}-wrong` })).status, 401);
  assert.equal((await request(running.port, 'GET', '/read', { token })).status, 200);
  assert.equal((await request(running.port, 'POST', '/run', { body: { command: 'echo fixture' } })).status, 401);
  assert.equal(
    (await request(running.port, 'POST', '/run', { body: { command: 'echo fixture' }, token })).status,
    200,
  );
  assert.equal((await request(running.port, 'POST', '/token', { body: { token } })).status, 409);
  assert.equal(running.output().includes(token), false);

  await stop(running.child);
  running = await startShim(home, fakeBin, shimPath);
  assert.equal((await request(running.port, 'GET', '/healthz')).status, 200);
  assert.equal((await request(running.port, 'GET', '/read')).status, 401);
  assert.equal((await request(running.port, 'GET', '/read', { token })).status, 200);
  assert.equal((await request(running.port, 'POST', '/token', { body: { token } })).status, 409);
});

test('healthz bounds Herdr calls and reuses a recent successful check', async (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'exec-shim-health-'));
  const home = path.join(root, 'home');
  const fakeBin = path.join(root, 'bin');
  const shimPath = path.join(root, 'exec-shim.js');
  const logPath = path.join(root, 'herdr-calls.log');
  fs.mkdirSync(home);
  fs.mkdirSync(fakeBin);
  fs.copyFileSync(shim, shimPath);
  const herdr = path.join(fakeBin, 'herdr');
  fs.writeFileSync(
    herdr,
    '#!/bin/sh\nif [ -n "${EXEC_SHIM_TEST_LOG:-}" ]; then printf "%s %s\\n" "$3" "$4" >> "$EXEC_SHIM_TEST_LOG"; fi\nif [ "$3" = "status" ] && [ "$4" = "server" ]; then if [ "${HERDR_TEST_SLOW_STATUS:-}" = "1" ]; then exec sleep 5; fi; echo "status: running"; exit 0; fi\nif [ "$3" = "pane" ] && [ "$4" = "read" ]; then echo "fixture pane"; exit 0; fi\nexit 0\n',
    { mode: 0o700 },
  );

  const running = await startShim(home, fakeBin, shimPath, {
    EXEC_SHIM_TEST_LOG: logPath,
  });
  t.after(async () => {
    await stop(running.child);
    fs.rmSync(root, { recursive: true, force: true });
  });

  assert.equal((await request(running.port, 'GET', '/healthz')).status, 200);
  assert.equal((await request(running.port, 'GET', '/healthz')).status, 200);
  assert.deepEqual(fs.readFileSync(logPath, 'utf8').trim().split('\n'), [
    'status server',
    'pane read',
  ]);

  await stop(running.child);
  const slow = await startShim(home, fakeBin, shimPath, {
    HERDR_TEST_SLOW_STATUS: '1',
  });
  t.after(async () => stop(slow.child));
  const startedAt = Date.now();
  assert.equal((await request(slow.port, 'GET', '/healthz')).status, 503);
  assert.ok(Date.now() - startedAt < 4000, 'hung Herdr status must be bounded by execFile timeout');
});
