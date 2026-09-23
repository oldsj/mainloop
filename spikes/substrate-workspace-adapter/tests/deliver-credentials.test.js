'use strict';

import assert from 'node:assert/strict';
import fs from 'node:fs';
import http from 'node:http';
import { createRequire } from 'node:module';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { once } from 'node:events';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const require = createRequire(import.meta.url);
const { buildCredentialPayload, deliverFromMountedFiles, postViaRouter } = require(
  path.resolve(__dirname, '../tools/phase4/deliver-credentials.cjs'),
);
const liveAgentImage = path.resolve(__dirname, '../live-agent-image');

test('credential payload uses the fixed shim allowlist and validates Codex auth JSON', () => {
  assert.deepEqual(buildCredentialPayload('claude', 'fixture token\r\n'), {
    name: 'claude-token',
    contents: 'fixturetoken',
  });
  assert.deepEqual(buildCredentialPayload('codex', '{"access_token":"fixture"}'), {
    name: 'codex-auth',
    contents: '{"access_token":"fixture"}',
  });
  assert.throws(() => buildCredentialPayload('../../etc/passwd', 'fixture'), /unsupported/);
  assert.throws(() => buildCredentialPayload('codex', 'not-json'), SyntaxError);
});

test('control delivery reads mounted paths and sends credential contents only in the request payload', async () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'phase4-delivery-'));
  const credentialFile = path.join(root, 'credential');
  const shimTokenFile = path.join(root, 'shim-token');
  fs.writeFileSync(credentialFile, 'fixture-claude-token\n', { mode: 0o600 });
  fs.writeFileSync(shimTokenFile, 'fixture-shim-token-with-at-least-32-characters', { mode: 0o600 });
  const calls = [];
  try {
    const result = await deliverFromMountedFiles({
      kind: 'claude',
      credentialFile,
      shimTokenFile,
      namespace: 'native-claude',
      actor: 'claude-final',
      request: async (request) => {
        calls.push(request);
        return 201;
      },
    });
    assert.deepEqual(result, {
      kind: 'claude',
      namespace: 'native-claude',
      actor: 'claude-final',
    });
    assert.equal(calls.length, 1);
    assert.equal(calls[0].payload.name, 'claude-token');
    assert.equal(calls[0].payload.contents, 'fixture-claude-token');
    assert.equal(calls[0].token, 'fixture-shim-token-with-at-least-32-characters');
    assert.equal(calls[0].namespace, 'native-claude');
    assert.equal(calls[0].actor, 'claude-final');
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});

test('router delivery authenticates the CONNECT target and posts the body without logging it', async (t) => {
  const received = {};
  const proxy = http.createServer();
  proxy.on('connect', (request, socket, head) => {
    received.target = request.url;
    received.actorHeader = request.headers['ate-target-actor'];
    if (head.length) socket.unshift(head);
    socket.write('HTTP/1.1 200 Connection Established\r\n\r\n');
    let bytes = Buffer.alloc(0);
    socket.on('data', (chunk) => {
      bytes = Buffer.concat([bytes, chunk]);
      const boundary = bytes.indexOf('\r\n\r\n');
      if (boundary === -1) return;
      const headers = bytes.subarray(0, boundary).toString('latin1');
      const length = Number(/^content-length:\s*(\d+)\s*$/im.exec(headers)?.[1]);
      if (!Number.isFinite(length) || bytes.length < boundary + 4 + length) return;
      received.requestHeaders = headers;
      received.payload = JSON.parse(bytes.subarray(boundary + 4, boundary + 4 + length).toString('utf8'));
      socket.end('HTTP/1.1 201 Created\r\nContent-Length: 0\r\nConnection: close\r\n\r\n');
    });
  });
  proxy.listen(0, '127.0.0.1');
  await once(proxy, 'listening');
  t.after(() => proxy.close());

  const { port } = proxy.address();
  const status = await postViaRouter({
    host: '127.0.0.1',
    port,
    namespace: 'native-codex',
    actor: 'codex-final',
    token: 'fixture-shim-token-with-at-least-32-characters',
    payload: { name: 'codex-auth', contents: '{"fixture":"provider"}' },
  });
  assert.equal(status, 201);
  assert.equal(received.target, 'actor-upstream:8090');
  assert.equal(received.actorHeader, 'native-codex/codex-final');
  assert.match(received.requestHeaders, /Authorization: Bearer fixture-shim-token-with-at-least-32-characters/i);
  assert.deepEqual(received.payload, { name: 'codex-auth', contents: '{"fixture":"provider"}' });
});

test('native-agent launcher reads Claude auth from its file into the process environment only', (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'phase4-launch-claude-'));
  const home = path.join(root, 'home');
  const bin = path.join(root, 'bin');
  const workspace = path.join(root, 'repo');
  const tokenFile = path.join(home, '.mainloop', 'claude-token');
  const tokenCapture = path.join(root, 'token.capture');
  const argsCapture = path.join(root, 'args.capture');
  fs.mkdirSync(path.dirname(tokenFile), { recursive: true });
  fs.mkdirSync(bin);
  fs.mkdirSync(workspace);
  fs.writeFileSync(tokenFile, 'fixture-claude-oauth-value\r\n', { mode: 0o600 });
  fs.writeFileSync(
    path.join(bin, 'claude'),
    '#!/bin/sh\nprintf "%s" "$CLAUDE_CODE_OAUTH_TOKEN" >"$TOKEN_CAPTURE"\nprintf "%s\\n" "$DISABLE_TELEMETRY" "$DISABLE_ERROR_REPORTING" "$CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC" "$DISABLE_AUTOUPDATER" >"$FLAGS_CAPTURE"\nprintf "%s\\n" "$@" >"$ARGS_CAPTURE"\n',
    { mode: 0o700 },
  );
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));

  const result = spawnSync('/bin/bash', [path.join(liveAgentImage, 'bin/start-native-agent'), 'claude'], {
    encoding: 'utf8',
    env: {
      ...process.env,
      HOME: home,
      PATH: `${bin}:${process.env.PATH}`,
      WORKSPACE_PATH: workspace,
      TOKEN_CAPTURE: tokenCapture,
      FLAGS_CAPTURE: path.join(root, 'flags.capture'),
      ARGS_CAPTURE: argsCapture,
      AGENT_SYSTEM_PROMPT_FILE: path.join(root, 'system-prompt.fixture'),
    },
  });
  assert.equal(result.status, 0, result.stderr);
  assert.equal(fs.readFileSync(tokenCapture, 'utf8'), 'fixture-claude-oauth-value');
  assert.deepEqual(fs.readFileSync(path.join(root, 'flags.capture'), 'utf8').trim().split('\n'), [
    '1', '1', '1', '1',
  ]);
  const args = fs.readFileSync(argsCapture, 'utf8');
  assert.match(args, /--dangerously-skip-permissions/);
  assert.match(args, /--append-system-prompt-file/);
  assert.equal(args.includes('fixture-claude-oauth-value'), false);
  assert.equal(result.stdout.includes('fixture-claude-oauth-value'), false);
});

test('native-agent launcher starts Codex only when its installed auth file exists', (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'phase4-launch-codex-'));
  const home = path.join(root, 'home');
  const codexHome = path.join(home, '.codex');
  const bin = path.join(root, 'bin');
  const workspace = path.join(root, 'repo');
  const argsCapture = path.join(root, 'args.capture');
  fs.mkdirSync(codexHome, { recursive: true });
  fs.mkdirSync(bin);
  fs.mkdirSync(workspace);
  fs.writeFileSync(path.join(codexHome, 'auth.json'), '{"fixture":"codex-auth"}', { mode: 0o600 });
  fs.writeFileSync(path.join(bin, 'codex'), '#!/bin/sh\nprintf "%s\\n" "$@" >"$ARGS_CAPTURE"\n', { mode: 0o700 });
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));

  const env = {
    ...process.env,
    HOME: home,
    CODEX_HOME: codexHome,
    PATH: `${bin}:${process.env.PATH}`,
    WORKSPACE_PATH: workspace,
    ARGS_CAPTURE: argsCapture,
  };
  const result = spawnSync('/bin/bash', [path.join(liveAgentImage, 'bin/start-native-agent'), 'codex'], {
    encoding: 'utf8',
    env,
  });
  assert.equal(result.status, 0, result.stderr);
  assert.equal(fs.readFileSync(argsCapture, 'utf8').trim(), '--dangerously-bypass-approvals-and-sandbox');
  assert.equal(result.stdout.includes('fixture'), false);

  fs.rmSync(path.join(codexHome, 'auth.json'));
  const missing = spawnSync('/bin/bash', [path.join(liveAgentImage, 'bin/start-native-agent'), 'codex'], {
    encoding: 'utf8',
    env,
  });
  assert.equal(missing.status, 1);
  assert.equal(missing.stderr.includes('auth.json'), false);
});

test('golden boot seeds trusted workspaces, themes, and disabled update checks without starting a CLI', (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'phase4-config-'));
  const home = path.join(root, 'home');
  const codexHome = path.join(home, '.codex');
  const workspace = path.join(root, 'repo');
  const result = spawnSync(process.execPath, [path.join(liveAgentImage, 'bin/prepare-native-agent-config.cjs')], {
    encoding: 'utf8',
    env: { ...process.env, HOME: home, CODEX_HOME: codexHome, WORKSPACE_PATH: workspace },
  });
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  assert.equal(result.status, 0, result.stderr);

  const claudeConfig = JSON.parse(fs.readFileSync(path.join(home, '.claude.json'), 'utf8'));
  assert.equal(claudeConfig.hasCompletedOnboarding, true);
  assert.equal(claudeConfig.theme, 'dark');
  assert.equal(claudeConfig.projects[workspace].hasTrustDialogAccepted, true);
  const codexConfig = fs.readFileSync(path.join(codexHome, 'config.toml'), 'utf8');
  assert.match(codexConfig, /^check_for_update_on_startup = false$/m);
  assert.match(codexConfig, /^theme = "dark"$/m);
  assert.ok(codexConfig.includes(`[projects.${JSON.stringify(workspace)}]`));
  assert.match(codexConfig, /^trust_level = "trusted"$/m);
});
