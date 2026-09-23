// Minimal preview-gate command executor: POST /run { command } pastes `command` as literal
// text into a real Herdr shell pane, and GET /read returns its current terminal buffer.
// Authenticated POST /credential { name, contents } writes only one of the fixed credential
// files used by the native-agent launchers. Request bodies are never logged.
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
const HEALTH_COMMAND_TIMEOUT_MS = 1500;
const HEALTH_CACHE_MS = 3000;
const MAX_REQUEST_BODY_BYTES = 64 * 1024;
const MAX_CREDENTIAL_REQUEST_BODY_BYTES = 512 * 1024;
const MAX_CREDENTIAL_BYTES = 64 * 1024;
if (!PANE_ID || !SESSION) {
  console.error('exec-shim: EXEC_SHIM_PANE_ID and HERDR_SESSION are required');
  process.exit(1);
}

const tokenPath = path.join(process.env.HOME || '/home/agent', '.mainloop', 'exec-shim-token');
const homePath = path.resolve(process.env.HOME || '/home/agent');
const codexHomePath = path.resolve(process.env.CODEX_HOME || path.join(homePath, '.codex'));
const credentialPaths = new Map([
  ['claude-token', path.join(homePath, '.mainloop', 'claude-token')],
  ['codex-auth', path.join(codexHomePath, 'auth.json')],
]);
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

function requestBody(req, res, onBody, maxBytes = MAX_REQUEST_BODY_BYTES) {
  const chunks = [];
  let size = 0;
  let tooLarge = false;
  req.on('data', (chunk) => {
    size += chunk.length;
    if (size > maxBytes) {
      if (!tooLarge) res.writeHead(413).end('request too large');
      tooLarge = true;
      return;
    }
    if (!tooLarge) chunks.push(chunk);
  });
  req.on('end', () => {
    if (!tooLarge) onBody(Buffer.concat(chunks).toString('utf8'));
  });
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

function installCredential(name, contents) {
  const destination = credentialPaths.get(name);
  if (!destination) return false;
  if (
    typeof contents !== 'string' ||
    !contents ||
    Buffer.byteLength(contents, 'utf8') > MAX_CREDENTIAL_BYTES
  ) return null;
  if (name === 'codex-auth') {
    let auth;
    try {
      auth = JSON.parse(contents);
    } catch {
      return null;
    }
    if (!auth || typeof auth !== 'object' || Array.isArray(auth)) return null;
  }

  const directory = path.dirname(destination);
  fs.mkdirSync(directory, { recursive: true, mode: 0o700 });
  const directoryStat = fs.lstatSync(directory);
  if (!directoryStat.isDirectory() || directoryStat.isSymbolicLink()) return null;
  fs.chmodSync(directory, 0o700);
  let fd;
  try {
    fd = fs.openSync(destination, 'wx', 0o600);
  } catch (err) {
    if (err.code === 'EEXIST') return 'exists';
    throw err;
  }
  try {
    fs.writeFileSync(fd, contents, 'utf8');
    fs.fsyncSync(fd);
    fs.closeSync(fd);
    fs.chmodSync(destination, 0o600);
  } catch (err) {
    try { fs.closeSync(fd); } catch {}
    fs.rmSync(destination, { force: true });
    throw err;
  }
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

function runHealthCheck() {
  return new Promise((resolve, reject) => {
    execFile(
      'herdr',
      ['--session', SESSION, 'status', 'server'],
      { timeout: HEALTH_COMMAND_TIMEOUT_MS },
      (statusErr, stdout) => {
        if (statusErr || !/^status:\s+running\s*$/m.test(stdout)) {
          reject(statusErr || new Error('Herdr server is not running'));
          return;
        }
        execFile(
          'herdr',
          ['--session', SESSION, 'pane', 'read', PANE_ID],
          { timeout: HEALTH_COMMAND_TIMEOUT_MS },
          (paneErr) => {
            if (paneErr) {
              reject(paneErr);
              return;
            }
            resolve();
          },
        );
      },
    );
  });
}

let lastGoodHealthAt = 0;
let healthCheckInFlight = null;

function healthz(res) {
  const respond = (ready) => {
    if (res.destroyed) return;
    if (ready) {
      // The shim is responsive, Herdr reports a running server, and the shell pane exists.
      // Never return status output or pane contents.
      res.writeHead(200, { 'content-type': 'text/plain' }).end('ok');
    } else {
      res.writeHead(503, { 'content-type': 'text/plain' }).end('not ready');
    }
  };

  if (Date.now() - lastGoodHealthAt < HEALTH_CACHE_MS) {
    respond(true);
    return;
  }
  if (!healthCheckInFlight) {
    healthCheckInFlight = runHealthCheck()
      .then(() => {
        lastGoodHealthAt = Date.now();
      })
      .finally(() => {
        healthCheckInFlight = null;
      });
  }
  healthCheckInFlight.then(() => respond(true), () => respond(false));
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
    requestBody(req, res, (body) => {
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
  if (req.method === 'POST' && req.url === '/credential') {
    if (bearerToken === null || !authorized(req)) return unauthorized(res);
    requestBody(req, res, (body) => {
      let document;
      try {
        document = JSON.parse(body);
      } catch {
        res.writeHead(400).end('invalid json');
        return;
      }
      if (!document || typeof document !== 'object' || Array.isArray(document)) {
        res.writeHead(400).end('invalid credential');
        return;
      }
      try {
        const installed = installCredential(document.name, document.contents);
        if (installed === null) {
          res.writeHead(400).end('invalid credential');
          return;
        }
        if (!installed) {
          res.writeHead(403).end('credential name is not allowlisted');
          return;
        }
        if (installed === 'exists') {
          res.writeHead(409).end('credential file already exists');
          return;
        }
        res.writeHead(201).end('credential stored');
      } catch {
        res.writeHead(500).end('credential could not be stored');
      }
    }, MAX_CREDENTIAL_REQUEST_BODY_BYTES);
    return;
  }
  if (req.method !== 'POST' || req.url !== '/run') {
    res.writeHead(404).end();
    return;
  }
  if (!authorized(req)) return unauthorized(res);
  requestBody(req, res, (body) => {
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
