// Minimal generic command executor for the preview-gate spike: POST /run { command } pastes
// `command` as literal text into a real Herdr shell pane via `herdr pane run` (fire-and-forget;
// the pane is a real bash shell, so this is a real shell write, not a purpose-built edit
// endpoint). GET /read returns the pane's current terminal buffer (`herdr pane read`), since
// `pane run` itself never captures output. Stands in for a credentialed native agent's own Bash
// tool -- see entrypoint.sh and docs/spikes/substrate-workspace-adapter.md for why a real agent
// could not be used here.
// Listens on port 8090, separate from the Vite dev server's port 80. Reached only through
// atenet-router's arbitrary-port CONNECT tunnel with the ate-target-actor header (see
// docs/api-guide.md "Workload Connectivity"), from test orchestration on the host -- never
// through the previewed route a browser uses (port 80 via the NGINX header-proxy).
'use strict';
const http = require('node:http');
const { execFile } = require('node:child_process');
const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');

const PANE_ID = process.env.EXEC_SHIM_PANE_ID;
const SESSION = process.env.HERDR_SESSION;
if (!PANE_ID || !SESSION) {
  console.error('exec-shim: EXEC_SHIM_PANE_ID and HERDR_SESSION are required');
  process.exit(1);
}

const tokenPath = path.join(process.env.HOME || '/home/agent', '.mainloop', 'exec-shim-token');
let bearerToken = null;
try {
  bearerToken = fs.readFileSync(tokenPath, 'utf8');
} catch (err) {
  if (err.code !== 'ENOENT') throw err;
}

function authorized(req) {
  if (bearerToken === null) return true;
  const header = req.headers.authorization;
  if (typeof header !== 'string' || !header.startsWith('Bearer ')) return false;
  const provided = Buffer.from(header.slice('Bearer '.length));
  const expected = Buffer.from(bearerToken);
  return provided.length === expected.length && crypto.timingSafeEqual(provided, expected);
}

function unauthorized(res) {
  res.writeHead(401, { 'content-type': 'text/plain' }).end('unauthorized');
}

function requestBody(req, onBody) {
  let body = '';
  req.on('data', (chunk) => {
    body += chunk;
    if (body.length > 65536) req.destroy();
  });
  req.on('end', () => onBody(body));
}

function installToken(token) {
  if (bearerToken !== null) return false;
  if (typeof token !== 'string' || token.length < 32 || token.length > 4096) return null;
  const directory = path.dirname(tokenPath);
  fs.mkdirSync(directory, { recursive: true, mode: 0o700 });
  fs.chmodSync(directory, 0o700);
  const fd = fs.openSync(tokenPath, 'wx', 0o600);
  try {
    fs.writeFileSync(fd, token, 'utf8');
    fs.fsyncSync(fd);
  } catch (err) {
    fs.closeSync(fd);
    fs.rmSync(tokenPath, { force: true });
    throw err;
  }
  fs.closeSync(fd);
  bearerToken = token;
  return true;
}

function herdr(args, res) {
  execFile('herdr', ['--session', SESSION, ...args], (err, stdout, stderr) => {
    if (err) {
      res.writeHead(502).end(String(err));
      return;
    }
    res
      .writeHead(200, { 'content-type': 'application/json' })
      .end(JSON.stringify({ ok: true, stdout, stderr }));
  });
}

function healthz(res) {
  execFile('herdr', ['--session', SESSION, 'status', 'server'], (statusErr, stdout) => {
    if (statusErr || !/^status:\s+running\s*$/m.test(stdout)) {
      res.writeHead(503, { 'content-type': 'text/plain' }).end('not ready');
      return;
    }
    execFile('herdr', ['--session', SESSION, 'pane', 'read', PANE_ID], (paneErr) => {
      if (paneErr) {
        res.writeHead(503, { 'content-type': 'text/plain' }).end('not ready');
        return;
      }
      // The request is served by this shim, Herdr reports a running server, and pane read
      // confirms the shell pane still exists. Do not expose status output or pane contents.
      res.writeHead(200, { 'content-type': 'text/plain' }).end('ok');
    });
  });
}

const server = http.createServer((req, res) => {
  if (req.method === 'GET' && req.url === '/healthz') {
    healthz(res);
    return;
  }
  if (req.method === 'POST' && req.url === '/token') {
    if (bearerToken !== null) {
      res.writeHead(409).end('token already set');
      return;
    }
    requestBody(req, (body) => {
      let token;
      try {
        token = JSON.parse(body).token;
      } catch {
        res.writeHead(400).end('invalid json');
        return;
      }
      try {
        const installed = installToken(token);
        if (installed === null) {
          res.writeHead(400).end('invalid token');
          return;
        }
        if (!installed) {
          res.writeHead(409).end('token already set');
          return;
        }
        res.writeHead(201).end('token set');
      } catch {
        res.writeHead(500).end('token could not be stored');
      }
    });
    return;
  }
  if (req.method === 'GET' && req.url === '/read') {
    if (!authorized(req)) return unauthorized(res);
    herdr(['pane', 'read', PANE_ID], res);
    return;
  }
  if (req.method !== 'POST' || req.url !== '/run') {
    res.writeHead(404).end();
    return;
  }
  if (!authorized(req)) return unauthorized(res);
  requestBody(req, (body) => {
    let command;
    try {
      command = JSON.parse(body).command;
    } catch {
      res.writeHead(400).end('invalid json');
      return;
    }
    if (typeof command !== 'string' || !command) {
      res.writeHead(400).end('missing command');
      return;
    }
    herdr(['pane', 'run', PANE_ID, command], res);
  });
});

server.listen(Number(process.env.EXEC_SHIM_PORT || 8090), '0.0.0.0', () => {
  const address = server.address();
  console.log(`exec-shim listening on :${address.port}`);
});
