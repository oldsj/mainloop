'use strict';

const fs = require('node:fs');
const http = require('node:http');

const CREDENTIALS = Object.freeze({
  claude: Object.freeze({ name: 'claude-token' }),
  codex: Object.freeze({ name: 'codex-auth' }),
});

function validateActorIdentity(namespace, actor) {
  const dnsLabel = /^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$/;
  if (!dnsLabel.test(namespace || '') || !dnsLabel.test(actor || '')) {
    throw new Error('invalid actor identity');
  }
}

function buildCredentialPayload(kind, rawContents) {
  const credential = CREDENTIALS[kind];
  if (!credential) throw new Error('unsupported credential kind');
  if (typeof rawContents !== 'string' || !rawContents) {
    throw new Error('empty credential');
  }
  const contents = kind === 'claude' ? rawContents.replace(/[ \r\n]/g, '') : rawContents;
  if (!contents || Buffer.byteLength(contents, 'utf8') > 1024 * 1024) {
    throw new Error('invalid credential size');
  }
  if (kind === 'codex') {
    const parsed = JSON.parse(contents);
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
      throw new Error('invalid Codex auth document');
    }
  }
  return { name: credential.name, contents };
}

function postViaRouter({ host, port, namespace, actor, token, payload, timeoutMs = 10000 }) {
  validateActorIdentity(namespace, actor);
  if (typeof token !== 'string' || !token || /[\r\n]/.test(token)) {
    return Promise.reject(new Error('invalid shim token'));
  }
  const body = Buffer.from(JSON.stringify(payload), 'utf8');

  return new Promise((resolve, reject) => {
    let settled = false;
    const finish = (error, status) => {
      if (settled) return;
      settled = true;
      if (error) reject(error);
      else resolve(status);
    };

    const tunnel = http.request({
      host,
      port,
      method: 'CONNECT',
      path: 'actor-upstream:8090',
      headers: { 'ate-target-actor': `${namespace}/${actor}` },
    });
    tunnel.setTimeout(timeoutMs, () => tunnel.destroy(new Error('router timed out')));
    tunnel.once('error', (error) => finish(error));
    tunnel.once('connect', (response, socket, head) => {
      if (response.statusCode !== 200) {
        socket.destroy();
        finish(new Error('router rejected actor tunnel'));
        return;
      }
      if (head.length) socket.unshift(head);

      let responseHeaders = Buffer.alloc(0);
      socket.setTimeout(timeoutMs, () => socket.destroy(new Error('shim timed out')));
      socket.once('error', (error) => finish(error));
      socket.on('data', (chunk) => {
        responseHeaders = Buffer.concat([responseHeaders, chunk]);
        const boundary = responseHeaders.indexOf('\r\n\r\n');
        if (boundary === -1) return;
        const statusLine = responseHeaders
          .subarray(0, boundary)
          .toString('latin1')
          .split('\r\n', 1)[0];
        const match = /^HTTP\/1\.[01] (\d{3})(?: |$)/.exec(statusLine);
        if (!match) {
          finish(new Error('invalid shim response'));
          socket.destroy();
          return;
        }
        finish(null, Number(match[1]));
        socket.end();
      });

      const headers = Buffer.from([
        'POST /credential HTTP/1.1',
        'Host: actor-upstream:8090',
        `Authorization: Bearer ${token}`,
        'Content-Type: application/json',
        `Content-Length: ${body.length}`,
        'Connection: close',
        '',
        '',
      ].join('\r\n'), 'ascii');
      socket.write(Buffer.concat([headers, body]));
    });
    tunnel.end();
  });
}

async function deliverFromMountedFiles({
  kind,
  credentialFile,
  shimTokenFile,
  namespace,
  actor,
  host = 'atenet-router.ate-system.svc.cluster.local',
  port = 8081,
  request = postViaRouter,
}) {
  validateActorIdentity(namespace, actor);
  const payload = buildCredentialPayload(kind, fs.readFileSync(credentialFile, 'utf8'));
  const token = fs.readFileSync(shimTokenFile, 'utf8').trim();
  if (!token || token.length > 4096 || /\s/.test(token)) {
    throw new Error('invalid shim token');
  }
  const status = await request({
    host,
    port,
    namespace,
    actor,
    token,
    payload,
  });
  if (status !== 201) throw new Error('shim rejected credential delivery');
  return { kind, namespace, actor };
}

async function main() {
  await deliverFromMountedFiles({
    kind: process.env.CREDENTIAL_KIND,
    credentialFile: '/var/run/phase4-credentials/credential',
    shimTokenFile: '/var/run/phase4-shim-auth/token',
    namespace: process.env.ACTOR_NAMESPACE,
    actor: process.env.ACTOR_NAME,
    host: process.env.ROUTER_HOST || 'atenet-router.ate-system.svc.cluster.local',
    port: Number(process.env.ROUTER_PORT || '8081'),
  });
  process.stdout.write(`credential delivered for ${process.env.CREDENTIAL_KIND}\n`);
}

if (require.main === module) {
  main().catch(() => {
    process.stderr.write('credential delivery failed\n');
    process.exitCode = 1;
  });
}

module.exports = { buildCredentialPayload, deliverFromMountedFiles, postViaRouter };
