'use strict';

import assert from 'node:assert/strict';
import fs from 'node:fs';
import http from 'node:http';
import os from 'node:os';
import path from 'node:path';
import { spawn, spawnSync } from 'node:child_process';
import { once } from 'node:events';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const image = path.resolve(__dirname, '../live-agent-image');
const shim = path.join(image, 'exec-shim.js');
const launcher = path.join(image, 'bin/start-native-agent');
const dockerfile = path.join(image, 'Dockerfile');
const entrypoint = path.join(image, 'entrypoint.sh');
const fixtures = path.join(__dirname, 'fixtures/native');
const token = 'fixture-only-shim-token-long-enough-for-testing';
const claudePlaceholder = 'sk-ant-oat01-mainloop-egress-placeholder';
function codexPlaceholder(accountId = 'fixture-account') {
  const encode = (value) => Buffer.from(JSON.stringify(value)).toString('base64url');
  const jwt = `${encode({ alg: 'none' })}.${encode({ exp: 1_799_999_999 })}.synthetic`;
  return JSON.stringify({
    auth_mode: 'chatgpt',
    tokens: {
      id_token: jwt,
      access_token: jwt,
      refresh_token: '',
      account_id: accountId
    },
    last_refresh: '2026-09-24T00:00:00Z'
  });
}

async function startShim(root, extraEnv = {}) {
  const home = path.join(root, 'home');
  const workspace = path.join(root, 'repo');
  const state = path.join(root, 'state');
  const fakeBin = path.join(root, 'bin');
  const shimCopy = path.join(root, 'exec-shim.js');
  fs.mkdirSync(home, { recursive: true });
  fs.mkdirSync(workspace, { recursive: true });
  fs.mkdirSync(fakeBin, { recursive: true });
  fs.copyFileSync(shim, shimCopy);
  const child = spawn(process.execPath, [shimCopy], {
    env: {
      ...process.env,
      ...(process.getuid() === 0 ? { EXEC_SHIM_TEST_ALLOW_ROOT: '1' } : {}),
      ...extraEnv,
      HOME: home,
      CODEX_HOME: path.join(home, '.codex'),
      WORKSPACE_PATH: workspace,
      EXEC_SHIM_STATE_DIR: state,
      EXEC_SHIM_HOST: '127.0.0.1',
      EXEC_SHIM_PORT: '0',
      NATIVE_AGENT_LAUNCHER: launcher,
      PATH: `${fakeBin}:${process.env.PATH}`
    },
    stdio: ['ignore', 'pipe', 'pipe']
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
  return { child, port, output: () => output, home, workspace, fakeBin, state };
}

test('exec shim refuses UID 0 unless its explicit test-only override is set', () => {
  const source = `Object.defineProperty(process, 'getuid', { value: () => 0 }); require(${JSON.stringify(shim)});`;
  const result = spawnSync(process.execPath, ['-e', source], {
    encoding: 'utf8',
    env: { ...process.env, EXEC_SHIM_TEST_ALLOW_ROOT: '0' }
  });
  assert.equal(result.status, 1);
  assert.match(result.stderr, /exec-shim refuses to start as UID 0/);
});

test('actor image prepares its writable paths before dropping root privileges', () => {
  const imageDockerfile = fs.readFileSync(dockerfile, 'utf8');
  const imageEntrypoint = fs.readFileSync(entrypoint, 'utf8');
  assert.ok(imageDockerfile.includes('util-linux'));
  assert.match(imageDockerfile, /^USER 0:0$/m);
  const statePreparation = imageEntrypoint.indexOf(
    'mkdir -p "${HOME}" /work "${EXEC_SHIM_STATE_DIR}"'
  );
  const ownership = imageEntrypoint.indexOf('chown -R "${AGENT_UID}:${AGENT_GID}" "${HOME}" /work');
  const privilegeDrop = imageEntrypoint.indexOf('exec setpriv');
  const shimStart = imageEntrypoint.indexOf('node "${EXEC_SHIM}"');
  assert.ok(statePreparation >= 0 && statePreparation < ownership);
  assert.ok(ownership >= 0 && ownership < privilegeDrop);
  assert.ok(privilegeDrop >= 0 && privilegeDrop < shimStart);
  for (const option of [
    '--reuid "${AGENT_UID}"',
    '--regid "${AGENT_GID}"',
    '--init-groups',
    '--bounding-set=-all',
    '--no-new-privs'
  ]) {
    assert.ok(imageEntrypoint.includes(option), `entrypoint is missing ${option}`);
  }
  assert.equal(imageEntrypoint.includes('IS_SANDBOX'), false);
  assert.equal(imageDockerfile.includes('EXEC_SHIM_TEST_ALLOW_ROOT'), false);
});

function request(port, method, route, { body, bearer } = {}) {
  return new Promise((resolve, reject) => {
    const headers = {};
    if (body !== undefined) headers['content-type'] = 'application/json';
    if (bearer !== undefined) headers.authorization = `Bearer ${bearer}`;
    const req = http.request({ host: '127.0.0.1', port, method, path: route, headers }, (res) => {
      const chunks = [];
      res.on('data', (chunk) => chunks.push(chunk));
      res.on('end', () =>
        resolve({
          status: res.statusCode,
          body: Buffer.concat(chunks).toString('utf8')
        })
      );
    });
    req.once('error', reject);
    if (body !== undefined) req.end(JSON.stringify(body));
    else req.end();
  });
}

async function installToken(running) {
  const result = await request(running.port, 'POST', '/token', { body: { token } });
  assert.equal(result.status, 201, result.body);
}

async function installCredential(running, name, contents) {
  const result = await request(running.port, 'POST', '/credential', {
    bearer: token,
    body: { name, contents }
  });
  assert.equal(result.status, 201, result.body);
}

async function waitForJob(running, kind, id, bearer = token) {
  for (let attempt = 0; attempt < 100; attempt += 1) {
    const result = await request(running.port, 'GET', `/${kind}/${id}`, { bearer });
    assert.equal(result.status, 200, result.body);
    const job = JSON.parse(result.body);
    if (job.status !== 'running') return job;
    await new Promise((resolve) => setTimeout(resolve, 20));
  }
  throw new Error(`${kind} job ${id} did not finish`);
}

async function stop(child) {
  if (child.exitCode !== null || child.signalCode !== null) return;
  child.kill('SIGTERM');
  await once(child, 'exit');
}

function fakeCli(file, script) {
  fs.writeFileSync(file, script, { mode: 0o700 });
}

test('token is private and one-time; readiness is workspace-only and work routes require auth', async (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'exec-shim-token-'));
  let running = await startShim(root);
  t.after(async () => {
    await stop(running.child);
    fs.rmSync(root, { recursive: true, force: true });
  });

  assert.equal((await request(running.port, 'GET', '/healthz')).status, 200);
  assert.equal((await request(running.port, 'GET', '/readyz')).status, 200);
  assert.equal(
    (await request(running.port, 'POST', '/run', { body: { command: 'true' } })).status,
    401
  );
  assert.equal(
    (await request(running.port, 'POST', '/turn', { body: { agent: 'claude', prompt: 'fixture' } }))
      .status,
    401
  );
  await installToken(running);

  const tokenPath = path.join(running.home, '.mainloop', 'exec-shim-token');
  assert.equal(fs.statSync(tokenPath).mode & 0o777, 0o600);
  assert.equal(fs.statSync(path.dirname(tokenPath)).mode & 0o777, 0o700);
  assert.equal(fs.readFileSync(tokenPath, 'utf8'), token);
  assert.equal(
    (await request(running.port, 'GET', '/turn/00000000-0000-4000-8000-000000000000')).status,
    401
  );
  assert.equal(
    (
      await request(running.port, 'GET', '/turn/00000000-0000-4000-8000-000000000000', {
        bearer: `${token}-wrong`
      })
    ).status,
    401
  );
  assert.equal(
    (
      await request(running.port, 'GET', '/turn/00000000-0000-4000-8000-000000000000', {
        bearer: token
      })
    ).status,
    404
  );
  assert.equal((await request(running.port, 'POST', '/token', { body: { token } })).status, 409);
  assert.equal(running.output().includes(token), false);

  await stop(running.child);
  running = await startShim(root);
  assert.equal((await request(running.port, 'GET', '/healthz')).status, 200);
  assert.equal(
    (await request(running.port, 'GET', '/turn/00000000-0000-4000-8000-000000000000')).status,
    401
  );
  assert.equal(
    (
      await request(running.port, 'GET', '/turn/00000000-0000-4000-8000-000000000000', {
        bearer: token
      })
    ).status,
    404
  );
});

test('credential delivery remains token-gated, allowlisted, private, and one-time', async (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'exec-shim-credentials-'));
  const running = await startShim(root);
  t.after(async () => {
    await stop(running.child);
    fs.rmSync(root, { recursive: true, force: true });
  });

  const claudeContents = 'fixture-real-provider-token-never-logged';
  const authContents = JSON.stringify({
    auth_mode: 'chatgpt',
    tokens: {
      id_token: 'fixture.header.signature',
      access_token: 'fixture.header.signature',
      refresh_token: 'fixture-real-refresh-token',
      account_id: 'fixture-account'
    },
    last_refresh: '2026-09-24T00:00:00Z'
  });
  assert.equal(
    (
      await request(running.port, 'POST', '/credential', {
        body: { name: 'codex-auth', contents: authContents }
      })
    ).status,
    401
  );
  await installToken(running);
  assert.equal(
    (
      await request(running.port, 'POST', '/credential', {
        bearer: `${token}-wrong`,
        body: { name: 'claude-token', contents: claudeContents }
      })
    ).status,
    401
  );
  assert.equal(
    (
      await request(running.port, 'POST', '/credential', {
        bearer: token,
        body: { name: '../outside', contents: claudeContents }
      })
    ).status,
    403
  );
  const claudePath = path.join(running.home, '.mainloop', 'claude-token');
  assert.equal(
    (
      await request(running.port, 'POST', '/credential', {
        bearer: token,
        body: { name: 'claude-token', contents: claudeContents }
      })
    ).status,
    400
  );
  assert.equal(
    (
      await request(running.port, 'POST', '/credential', {
        bearer: token,
        body: { name: 'codex-auth', contents: '[]' }
      })
    ).status,
    400
  );
  assert.equal(
    (
      await request(running.port, 'POST', '/credential', {
        bearer: token,
        body: { name: 'claude-token', contents: claudePlaceholder }
      })
    ).status,
    201
  );
  assert.equal(fs.readFileSync(claudePath, 'utf8'), claudePlaceholder);
  assert.equal(fs.statSync(claudePath).mode & 0o777, 0o600);
  assert.equal(
    (
      await request(running.port, 'POST', '/credential', {
        bearer: token,
        body: { name: 'claude-token', contents: claudePlaceholder }
      })
    ).status,
    409
  );
  assert.equal(
    (
      await request(running.port, 'PUT', '/credential', {
        bearer: token,
        body: { name: 'claude-token', contents: claudePlaceholder }
      })
    ).status,
    200
  );
  assert.equal(fs.readFileSync(claudePath, 'utf8'), claudePlaceholder);
  assert.equal(
    (
      await request(running.port, 'PUT', '/credential', {
        bearer: token,
        body: { name: 'claude-token', contents: claudeContents }
      })
    ).status,
    400
  );
  const authPath = path.join(running.home, '.codex', 'auth.json');
  assert.equal(
    (
      await request(running.port, 'POST', '/credential', {
        bearer: token,
        body: { name: 'codex-auth', contents: authContents }
      })
    ).status,
    400
  );
  const placeholder = codexPlaceholder();
  assert.equal(
    (
      await request(running.port, 'POST', '/credential', {
        bearer: token,
        body: { name: 'codex-auth', contents: placeholder }
      })
    ).status,
    201
  );
  assert.equal(fs.readFileSync(authPath, 'utf8'), placeholder);
  assert.equal(fs.statSync(authPath).mode & 0o777, 0o600);
  const replaced = await request(running.port, 'PUT', '/credential', {
    bearer: token,
    body: { name: 'codex-auth', contents: placeholder }
  });
  assert.equal(replaced.status, 200, replaced.body);
  assert.equal(fs.readFileSync(authPath, 'utf8'), placeholder);
  const rejectedReplacement = await request(running.port, 'PUT', '/credential', {
    bearer: token,
    body: { name: 'codex-auth', contents: authContents }
  });
  assert.equal(rejectedReplacement.status, 400);
  assert.equal(fs.readFileSync(authPath, 'utf8'), placeholder);
  assert.equal(running.output().includes(claudeContents), false);
  assert.equal(running.output().includes(authContents), false);
});

test('turn prompt is piped on stdin and never appears in argv or shim logs', async (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'exec-shim-stdin-'));
  const running = await startShim(root, {
    ARGS_CAPTURE: path.join(root, 'claude-args'),
    PROMPT_CAPTURE: path.join(root, 'claude-prompt')
  });
  fakeCli(
    path.join(running.fakeBin, 'claude'),
    '#!/bin/sh\nprintf "%s\\n" "$@" >"$ARGS_CAPTURE"\ncat >"$PROMPT_CAPTURE"\ncat <<\'EVENTS\'\n{"type":"system","subtype":"init","session_id":"fixture-session"}\n{"type":"result","result":"fixture answer","session_id":"fixture-session"}\nEVENTS\n'
  );
  t.after(async () => {
    await stop(running.child);
    fs.rmSync(root, { recursive: true, force: true });
  });
  await installToken(running);
  await installCredential(running, 'claude-token', claudePlaceholder);
  const prompt = 'private fixture prompt must travel only on stdin';
  const started = await request(running.port, 'POST', '/turn', {
    bearer: token,
    body: {
      agent: 'claude',
      prompt,
      session_key: 'stdin-session',
      session_id: 'native-stdin-session',
      resume: false
    }
  });
  assert.equal(started.status, 202, started.body);
  const { id } = JSON.parse(started.body);
  const result = await waitForJob(running, 'turn', id);
  assert.equal(result.status, 'completed');
  assert.equal(result.final_message, 'fixture answer');
  assert.equal(result.native_session_id, 'fixture-session');
  assert.equal(fs.readFileSync(path.join(root, 'claude-prompt'), 'utf8'), prompt);
  const argv = fs.readFileSync(path.join(root, 'claude-args'), 'utf8');
  assert.match(argv, /-p/);
  assert.match(argv, /--session-id\s+native-stdin-session/);
  assert.match(argv, /--output-format/);
  assert.equal(argv.includes(prompt), false);
  assert.equal(running.output().includes(prompt), false);
  assert.equal(
    fs
      .readFileSync(path.join(running.state, 'turns', id + '.events.jsonl'), 'utf8')
      .includes(prompt),
    false
  );
});

test('a second concurrent turn for the same logical session receives 409', async (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'exec-shim-concurrency-'));
  const running = await startShim(root, { TURN_DELAY: '0.4' });
  fakeCli(
    path.join(running.fakeBin, 'claude'),
    '#!/bin/sh\ncat >/dev/null\nsleep "$TURN_DELAY"\nprintf \'%s\\n\' \'{"type":"result","session_id":"fixture-session","result":"done"}\'\n'
  );
  t.after(async () => {
    await stop(running.child);
    fs.rmSync(root, { recursive: true, force: true });
  });
  await installToken(running);
  await installCredential(running, 'claude-token', claudePlaceholder);
  const body = {
    agent: 'claude',
    prompt: 'turn prompt',
    session_key: 'same-session',
    session_id: 'native-same-session',
    resume: false
  };
  const first = await request(running.port, 'POST', '/turn', { bearer: token, body });
  assert.equal(first.status, 202, first.body);
  const second = await request(running.port, 'POST', '/turn', { bearer: token, body });
  assert.equal(second.status, 409);
  const result = await waitForJob(running, 'turn', JSON.parse(first.body).id);
  assert.equal(result.status, 'completed');
});

test('same-provider sessions scope status, cancellation, and interleaved sends', async (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'exec-shim-session-scope-'));
  const running = await startShim(root);
  fakeCli(
    path.join(running.fakeBin, 'claude'),
    '#!/bin/sh\nprompt="$(cat)"\ncase "$prompt" in\n  slow-A) native_id=native-A; sleep 5 ;;\n  slow-B) native_id=native-B; sleep 1 ;;\n  second-B) native_id=native-B ;;\n  *) exit 2 ;;\nesac\nprintf \'{"type":"result","session_id":"%s","result":"%s"}\\n\' "$native_id" "$prompt"\n'
  );
  t.after(async () => {
    await stop(running.child);
    fs.rmSync(root, { recursive: true, force: true });
  });
  await installToken(running);
  await installCredential(running, 'claude-token', claudePlaceholder);

  const sessionA = {
    agent: 'claude',
    prompt: 'slow-A',
    session_key: 'logical-session-A',
    session_id: 'native-A',
    resume: false
  };
  const sessionB = {
    agent: 'claude',
    prompt: 'slow-B',
    session_key: 'logical-session-B',
    session_id: 'native-B',
    resume: false
  };
  const startedA = await request(running.port, 'POST', '/turn', {
    bearer: token,
    body: sessionA
  });
  assert.equal(startedA.status, 202, startedA.body);
  const startedB = await request(running.port, 'POST', '/turn', {
    bearer: token,
    body: sessionB
  });
  assert.equal(startedB.status, 202, startedB.body);
  const jobA = JSON.parse(startedA.body).id;
  const jobB = JSON.parse(startedB.body).id;
  const statusA = await request(
    running.port,
    'GET',
    '/turn/status?agent=claude&session_key=logical-session-A',
    { bearer: token }
  );
  const statusB = await request(
    running.port,
    'GET',
    '/turn/status?agent=claude&session_key=logical-session-B',
    { bearer: token }
  );
  assert.equal(JSON.parse(statusA.body).id, jobA);
  assert.equal(JSON.parse(statusA.body).status, 'running');
  assert.equal(JSON.parse(statusB.body).id, jobB);
  assert.equal(JSON.parse(statusB.body).status, 'running');
  assert.equal(JSON.parse(statusB.body).native_session_id, 'native-B');

  const stoppedA = await request(running.port, 'POST', '/turn/stop', {
    bearer: token,
    body: { agent: 'claude', session_key: 'logical-session-A' }
  });
  assert.equal(JSON.parse(stoppedA.body).status, 'interrupted');
  assert.equal((await waitForJob(running, 'turn', jobA)).status, 'interrupted');
  const statusBAfterStop = await request(
    running.port,
    'GET',
    '/turn/status?agent=claude&session_key=logical-session-B',
    { bearer: token }
  );
  assert.equal(JSON.parse(statusBAfterStop.body).status, 'running');
  const completedB = await waitForJob(running, 'turn', jobB);
  assert.equal(completedB.final_message, 'slow-B');

  const secondB = await request(running.port, 'POST', '/turn', {
    bearer: token,
    body: { ...sessionB, prompt: 'second-B', resume: true }
  });
  assert.equal(secondB.status, 202, secondB.body);
  const completedSecondB = await waitForJob(running, 'turn', JSON.parse(secondB.body).id);
  assert.equal(completedSecondB.final_message, 'second-B');

  const finalStatusB = await request(
    running.port,
    'GET',
    '/turn/status?agent=claude&session_key=logical-session-B',
    { bearer: token }
  );
  assert.equal(JSON.parse(finalStatusB.body).id, JSON.parse(secondB.body).id);
  assert.equal(JSON.parse(finalStatusB.body).status, 'completed');
});

test('turn status locates the latest turn after it completes', async (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'exec-shim-status-'));
  const running = await startShim(root, {
    CLAUDE_EVENTS: path.join(fixtures, 'claude-stream-json.jsonl')
  });
  fakeCli(
    path.join(running.fakeBin, 'claude'),
    '#!/bin/sh\ncat >/dev/null\ncat "$CLAUDE_EVENTS"\n'
  );
  t.after(async () => {
    await stop(running.child);
    fs.rmSync(root, { recursive: true, force: true });
  });
  await installToken(running);
  await installCredential(running, 'claude-token', claudePlaceholder);

  const submitted = await request(running.port, 'POST', '/turn', {
    bearer: token,
    body: {
      agent: 'claude',
      prompt: 'fixture status prompt',
      session_key: 'status-session',
      session_id: 'native-status-session',
      resume: false
    }
  });
  assert.equal(submitted.status, 202, submitted.body);
  const { id } = JSON.parse(submitted.body);
  const current = await waitForJob(running, 'turn', id);
  const status = await request(
    running.port,
    'GET',
    '/turn/status?agent=claude&session_key=status-session',
    {
      bearer: token
    }
  );
  assert.equal(status.status, 200, status.body);
  assert.equal(JSON.parse(status.body).id, id);
  assert.equal(JSON.parse(status.body).status, 'completed');
  assert.equal(JSON.parse(status.body).native_session_id, current.native_session_id);
  assert.equal(
    (
      await request(running.port, 'GET', '/turn/status?agent=claude&session_key=other-session', {
        bearer: token
      })
    ).status,
    404
  );
});

test('turn status exposes only a boolean provider-auth rejection signal', async (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'exec-shim-auth-rejected-'));
  const running = await startShim(root);
  fakeCli(
    path.join(running.fakeBin, 'claude'),
    "#!/bin/sh\ncat >/dev/null\nprintf 'provider request failed with HTTP 401 Unauthorized\\n' >&2\nexit 1\n"
  );
  t.after(async () => {
    await stop(running.child);
    fs.rmSync(root, { recursive: true, force: true });
  });
  await installToken(running);
  await installCredential(running, 'claude-token', claudePlaceholder);

  const submitted = await request(running.port, 'POST', '/turn', {
    bearer: token,
    body: {
      agent: 'claude',
      prompt: 'synthetic rejected turn',
      session_key: 'rejected-session',
      session_id: 'native-rejected-session',
      resume: false
    }
  });
  assert.equal(submitted.status, 202, submitted.body);
  const { id } = JSON.parse(submitted.body);
  const result = await waitForJob(running, 'turn', id);
  assert.equal(result.status, 'failed');
  assert.equal(result.credential_rejected, true);
  assert.equal(running.output().includes(claudePlaceholder), false);
});

test('agent readiness requires its credential and rejects unauthenticated checks', async (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'exec-shim-agent-ready-'));
  const running = await startShim(root);
  t.after(async () => {
    await stop(running.child);
    fs.rmSync(root, { recursive: true, force: true });
  });
  await installToken(running);

  const missing = await request(running.port, 'GET', '/agent/ready?agent=codex', {
    bearer: token
  });
  assert.equal(missing.status, 503);
  const unauthorized = await request(running.port, 'GET', '/agent/ready?agent=codex');
  assert.equal(unauthorized.status, 401);

  await installCredential(running, 'codex-auth', codexPlaceholder());
  const configured = await request(running.port, 'GET', '/agent/ready?agent=codex', {
    bearer: token
  });
  assert.equal(configured.status, 200, configured.body);
  assert.deepEqual(JSON.parse(configured.body), { agent: 'codex', configured: true });
});

test('process registry starts and stops long-lived commands and reports bounded tails and ports', async (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'exec-shim-processes-'));
  const procNet = path.join(root, 'proc-net');
  fs.mkdirSync(procNet, { recursive: true });
  fs.writeFileSync(
    path.join(procNet, 'tcp'),
    'sl local_address rem_address st\n' +
      '0: 0100007F:1F90 00000000:0000 0A\n' +
      '1: 0100007F:1F91 00000000:0000 01\n'
  );
  fs.writeFileSync(
    path.join(procNet, 'tcp6'),
    'sl local_address rem_address st\n' +
      '0: 00000000000000000000000000000000:1FBB 00000000000000000000000000000000:0000 0A\n'
  );
  const running = await startShim(root, { EXEC_SHIM_PROC_NET_DIR: procNet });
  t.after(async () => {
    await stop(running.child);
    fs.rmSync(root, { recursive: true, force: true });
  });
  await installToken(running);

  assert.equal((await request(running.port, 'GET', '/processes')).status, 401);
  assert.equal((await request(running.port, 'GET', '/ports')).status, 401);
  const invalid = await request(running.port, 'POST', '/processes', {
    bearer: token,
    body: { command: '   ' }
  });
  assert.equal(invalid.status, 400);

  const started = await request(running.port, 'POST', '/processes', {
    bearer: token,
    body: {
      label: 'vite',
      command:
        "node -e \"process.stdout.write('x'.repeat(100000) + '\\\\npreview-ready\\\\n')\"; while :; do sleep 1; done"
    }
  });
  assert.equal(started.status, 201, started.body);
  const { id } = JSON.parse(started.body);

  let listed;
  for (let attempt = 0; attempt < 50; attempt += 1) {
    const result = await request(running.port, 'GET', '/processes', { bearer: token });
    assert.equal(result.status, 200, result.body);
    listed = JSON.parse(result.body).processes.find((entry) => entry.id === id);
    if (listed?.log_tail.includes('preview-ready')) break;
    await new Promise((resolve) => setTimeout(resolve, 20));
  }
  assert.equal(listed.status, 'running');
  assert.equal(listed.label, 'vite');
  assert.match(listed.log_tail, /preview-ready/);
  assert.ok(Buffer.byteLength(listed.log_tail) <= 64 * 1024);
  assert.equal(Object.hasOwn(listed, 'command'), false);
  assert.equal(running.output().includes('preview-ready'), false);

  const ports = await request(running.port, 'GET', '/ports', { bearer: token });
  assert.equal(ports.status, 200, ports.body);
  assert.deepEqual(JSON.parse(ports.body).ports, [8080, 8123]);

  const stopped = await request(running.port, 'DELETE', `/processes/${id}`, { bearer: token });
  assert.equal(stopped.status, 200, stopped.body);
  assert.equal(JSON.parse(stopped.body).status, 'stopped');
});

test('journal returns bounded numbered pages for Claude and Codex and rejects a bad token', async (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'exec-shim-journal-'));
  const running = await startShim(root);
  t.after(async () => {
    await stop(running.child);
    fs.rmSync(root, { recursive: true, force: true });
  });
  await installToken(running);

  const claudeId = 'fixture-claude-session';
  const claudeFile = path.join(running.home, '.claude', 'projects', 'p1', `${claudeId}.jsonl`);
  fs.mkdirSync(path.dirname(claudeFile), { recursive: true });
  fs.writeFileSync(claudeFile, '{"type":"system"}\n{"type":"user"}\n{"type":"assistant"}\npartial');
  const firstPage = await request(
    running.port,
    'GET',
    `/journal?agent=claude&id=${claudeId}&from=1&limit=1`,
    { bearer: token }
  );
  assert.equal(firstPage.status, 200, firstPage.body);
  assert.deepEqual(JSON.parse(firstPage.body), {
    file: claudeFile,
    total_lines: 3,
    lines: [{ line: 2, text: '{"type":"user"}' }]
  });
  const wrongToken = await request(
    running.port,
    'GET',
    `/journal?agent=claude&id=${claudeId}&from=0`,
    { bearer: `${token}-wrong` }
  );
  assert.equal(wrongToken.status, 401);

  const codexId = 'fixture-codex-thread';
  const codexFile = path.join(
    running.home,
    '.codex',
    'sessions',
    '2026',
    '09',
    'rollout-2026-09-24T00-00-00-fixture-codex-thread.jsonl'
  );
  fs.mkdirSync(path.dirname(codexFile), { recursive: true });
  fs.writeFileSync(codexFile, '{"type":"thread.started"}\n{"type":"turn.completed"}\n');
  const codexPage = await request(
    running.port,
    'GET',
    `/journal?agent=codex&id=${codexId}&from=0&limit=1`,
    { bearer: token }
  );
  assert.equal(codexPage.status, 200, codexPage.body);
  assert.deepEqual(JSON.parse(codexPage.body), {
    file: codexFile,
    total_lines: 2,
    lines: [{ line: 1, text: '{"type":"thread.started"}' }]
  });
  assert.equal(running.output().includes(token), false);
});

test('stop interrupts an in-flight turn and releases its slot', async (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'exec-shim-stop-'));
  const running = await startShim(root);
  fakeCli(path.join(running.fakeBin, 'claude'), '#!/bin/sh\ncat >/dev/null\nsleep 5\n');
  t.after(async () => {
    await stop(running.child);
    fs.rmSync(root, { recursive: true, force: true });
  });
  await installToken(running);
  await installCredential(running, 'claude-token', claudePlaceholder);

  const submitted = await request(running.port, 'POST', '/turn', {
    bearer: token,
    body: {
      agent: 'claude',
      prompt: 'stop fixture turn',
      session_key: 'stop-session',
      session_id: 'native-stop-session',
      resume: false
    }
  });
  assert.equal(submitted.status, 202, submitted.body);
  const { id } = JSON.parse(submitted.body);
  const stopped = await request(running.port, 'POST', '/turn/stop', {
    bearer: token,
    body: { agent: 'claude', session_key: 'stop-session' }
  });
  assert.equal(stopped.status, 200, stopped.body);
  assert.equal(JSON.parse(stopped.body).status, 'interrupted');
  const result = await waitForJob(running, 'turn', id);
  assert.equal(result.status, 'interrupted');
});

test('/run executes a command, stores bounded output, and reports timeout', async (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'exec-shim-run-'));
  const running = await startShim(root);
  t.after(async () => {
    await stop(running.child);
    fs.rmSync(root, { recursive: true, force: true });
  });
  await installToken(running);
  const quick = await request(running.port, 'POST', '/run', {
    bearer: token,
    body: { command: 'printf fixture-output', timeout_ms: 1000 }
  });
  assert.equal(quick.status, 202, quick.body);
  const completed = await waitForJob(running, 'run', JSON.parse(quick.body).id);
  assert.equal(completed.status, 'completed');
  assert.equal(completed.exit_code, 0);
  assert.equal(completed.output, 'fixture-output');

  const slow = await request(running.port, 'POST', '/run', {
    bearer: token,
    body: { command: 'sleep 5; printf should-not-finish', timeout_ms: 50 }
  });
  assert.equal(slow.status, 202, slow.body);
  const timeout = await waitForJob(running, 'run', JSON.parse(slow.body).id);
  assert.equal(timeout.status, 'timed_out');
  assert.equal(timeout.output.includes('should-not-finish'), false);
});

test('Claude stream-json and Codex JSONL produce native ids and final messages', async (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'exec-shim-events-'));
  const running = await startShim(root, {
    CLAUDE_EVENTS: path.join(fixtures, 'claude-stream-json.jsonl'),
    CODEX_EVENTS: path.join(fixtures, 'codex-jsonl.jsonl'),
    CLAUDE_ARGS_CAPTURE: path.join(root, 'claude-args'),
    CODEX_ARGS_CAPTURE: path.join(root, 'codex-args')
  });
  fakeCli(
    path.join(running.fakeBin, 'claude'),
    '#!/bin/sh\nprintf "%s\\n" "$@" >>"$CLAUDE_ARGS_CAPTURE"\ncat >/dev/null\ncat "$CLAUDE_EVENTS"\n'
  );
  fakeCli(
    path.join(running.fakeBin, 'codex'),
    '#!/bin/sh\nprintf "%s\\n" "$@" >"$CODEX_ARGS_CAPTURE"\ncat >/dev/null\ncat "$CODEX_EVENTS"\n'
  );
  t.after(async () => {
    await stop(running.child);
    fs.rmSync(root, { recursive: true, force: true });
  });
  await installToken(running);
  await installCredential(running, 'claude-token', claudePlaceholder);
  await installCredential(running, 'codex-auth', codexPlaceholder());

  const claudeStart = await request(running.port, 'POST', '/turn', {
    bearer: token,
    body: {
      agent: 'claude',
      session_key: 'events-claude',
      session_id: 'new-claude-session-001',
      resume: false,
      prompt: 'claude first turn'
    }
  });
  assert.equal(claudeStart.status, 202, claudeStart.body);
  const claude = await waitForJob(running, 'turn', JSON.parse(claudeStart.body).id);
  assert.equal(claude.native_session_id, 'fixture-claude-session');
  assert.equal(claude.final_message, 'fixture Claude final');
  assert.equal(claude.events.length, 3);
  const firstClaudeArgs = fs.readFileSync(path.join(root, 'claude-args'), 'utf8');
  assert.match(firstClaudeArgs, /--session-id\s+new-claude-session-001/);

  const resumedClaudeStart = await request(running.port, 'POST', '/turn', {
    bearer: token,
    body: {
      agent: 'claude',
      session_key: 'events-claude',
      session_id: 'fixture-claude-session',
      resume: true,
      prompt: 'claude follow-up turn'
    }
  });
  assert.equal(resumedClaudeStart.status, 202, resumedClaudeStart.body);
  await waitForJob(running, 'turn', JSON.parse(resumedClaudeStart.body).id);
  const claudeArgs = fs.readFileSync(path.join(root, 'claude-args'), 'utf8');
  assert.match(claudeArgs, /--resume\s+fixture-claude-session/);

  const invalidResume = await request(running.port, 'POST', '/turn', {
    bearer: token,
    body: {
      agent: 'claude',
      session_key: 'invalid-resume',
      resume: true,
      prompt: 'must not start'
    }
  });
  assert.equal(invalidResume.status, 400);

  const codexStart = await request(running.port, 'POST', '/turn', {
    bearer: token,
    body: {
      agent: 'codex',
      session_key: 'events-codex',
      session_id: 'resume-thread-001',
      resume: true,
      prompt: 'codex fixture prompt'
    }
  });
  assert.equal(codexStart.status, 202, codexStart.body);
  const codex = await waitForJob(running, 'turn', JSON.parse(codexStart.body).id);
  assert.equal(codex.native_session_id, 'fixture-codex-thread');
  assert.equal(codex.final_message, 'fixture Codex final');
  assert.equal(codex.events.length, 4);
  assert.equal(
    fs.readFileSync(path.join(root, 'codex-args'), 'utf8').trim(),
    'exec\nresume\n--json\nresume-thread-001\n--dangerously-bypass-approvals-and-sandbox'
  );
});
