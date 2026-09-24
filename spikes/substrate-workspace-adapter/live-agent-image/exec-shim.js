// Authenticated, headless turn and command service for the Substrate actor.
// Prompts are piped to the native CLI on stdin and are never logged or persisted.
'use strict';
const http = require('node:http');
const { spawn } = require('node:child_process');
const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');

const MAX_REQUEST_BODY_BYTES = 64 * 1024;
const MAX_CREDENTIAL_REQUEST_BODY_BYTES = 512 * 1024;
const MAX_CREDENTIAL_BYTES = 64 * 1024;
const MAX_JOB_OUTPUT_BYTES = 2 * 1024 * 1024;
const MAX_RESPONSE_OUTPUT_BYTES = 32 * 1024;
const MAX_RETURN_EVENTS = 256;
const MAX_JOURNAL_LINES = 200;
const MAX_JOURNAL_LINE_BYTES = 1024 * 1024;
const MAX_JOURNAL_RESPONSE_BYTES = 2 * 1024 * 1024;
const MAX_JOURNAL_SEARCH_ENTRIES = 100_000;
const MAX_LONG_PROCESSES = 32;
const MAX_LONG_PROCESS_LOG_BYTES = 64 * 1024;
const MAX_LONG_PROCESS_RECORDS = 256;
const DEFAULT_RUN_TIMEOUT_MS = 60_000;
const DEFAULT_TURN_TIMEOUT_MS = 10 * 60_000;
const MAX_TIMEOUT_MS = 10 * 60_000;
const WORKSPACE_PATH = path.resolve(process.env.WORKSPACE_PATH || '/work/repo');
const HOME_PATH = path.resolve(process.env.HOME || '/home/agent');
const CODEX_HOME_PATH = path.resolve(process.env.CODEX_HOME || path.join(HOME_PATH, '.codex'));
const STATE_DIR = path.resolve(
  process.env.EXEC_SHIM_STATE_DIR || path.join(WORKSPACE_PATH, '.mainloop')
);
const PROC_NET_DIR = path.resolve(process.env.EXEC_SHIM_PROC_NET_DIR || '/proc/net');
const RUN_DIR = path.join(STATE_DIR, 'runs');
const TURN_DIR = path.join(STATE_DIR, 'turns');
const LAUNCHER = process.env.NATIVE_AGENT_LAUNCHER || '/usr/local/bin/start-native-agent';

if (
  typeof process.getuid === 'function' &&
  process.getuid() === 0 &&
  process.env.EXEC_SHIM_TEST_ALLOW_ROOT !== '1'
) {
  process.stderr.write('exec-shim refuses to start as UID 0\n');
  process.exit(1);
}

for (const directory of [STATE_DIR, RUN_DIR, TURN_DIR]) {
  fs.mkdirSync(directory, { recursive: true, mode: 0o700 });
  fs.chmodSync(directory, 0o700);
}

const tokenPath = path.join(HOME_PATH, '.mainloop', 'exec-shim-token');
const credentialPaths = new Map([
  ['claude-token', path.join(HOME_PATH, '.mainloop', 'claude-token')],
  ['codex-auth', path.join(CODEX_HOME_PATH, 'auth.json')]
]);
let bearerToken = null;
try {
  bearerToken = fs.readFileSync(tokenPath, 'utf8');
} catch (err) {
  if (err.code !== 'ENOENT') throw err;
}

const jobs = new Map();
const longProcesses = new Map();
const activeTurns = new Map();
const latestTurns = new Map();
const turnProcesses = new Map();

function json(res, status, document) {
  res.writeHead(status, { 'content-type': 'application/json' }).end(JSON.stringify(document));
}

function authorized(req) {
  if (bearerToken === null) return false;
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
    if (!tooLarge && !res.destroyed) onBody(Buffer.concat(chunks).toString('utf8'));
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

function installCredential(name, contents, replace = false) {
  const destination = credentialPaths.get(name);
  if (!destination || (replace && !['codex-auth', 'claude-token'].includes(name))) return false;
  if (
    typeof contents !== 'string' ||
    !contents ||
    Buffer.byteLength(contents, 'utf8') > MAX_CREDENTIAL_BYTES
  )
    return null;
  if (!isPlaceholderCredential(name, contents)) {
    return null;
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
    if (err.code === 'EEXIST' && !replace) return 'exists';
    if (err.code === 'EEXIST' && replace) {
      const temporary = destination + '.' + crypto.randomUUID() + '.tmp';
      try {
        fd = fs.openSync(temporary, 'wx', 0o600);
        fs.writeFileSync(fd, contents, 'utf8');
        fs.fsyncSync(fd);
        fs.closeSync(fd);
        fs.renameSync(temporary, destination);
        fs.chmodSync(destination, 0o600);
        return true;
      } catch (writeError) {
        try {
          if (fd !== undefined) fs.closeSync(fd);
        } catch {}
        fs.rmSync(temporary, { force: true });
        throw writeError;
      }
    }
    throw err;
  }
  try {
    fs.writeFileSync(fd, contents, 'utf8');
    fs.fsyncSync(fd);
    fs.closeSync(fd);
    fs.chmodSync(destination, 0o600);
  } catch (err) {
    try {
      fs.closeSync(fd);
    } catch {}
    fs.rmSync(destination, { force: true });
    throw err;
  }
  return true;
}

function isSyntheticJwt(value) {
  if (typeof value !== 'string') return false;
  const parts = value.split('.');
  if (parts.length !== 3 || parts[2] !== 'synthetic') return false;
  try {
    const header = JSON.parse(Buffer.from(parts[0], 'base64url').toString('utf8'));
    const payload = JSON.parse(Buffer.from(parts[1], 'base64url').toString('utf8'));
    return header?.alg === 'none' && Number.isSafeInteger(payload?.exp);
  } catch {
    return false;
  }
}

function isPlaceholderCredential(name, contents) {
  if (name === 'claude-token') {
    return contents === 'sk-ant-oat01-mainloop-egress-placeholder';
  }
  if (name !== 'codex-auth') return false;
  try {
    const auth = JSON.parse(contents);
    const tokens = auth?.tokens;
    return (
      auth?.auth_mode === 'chatgpt' &&
      typeof auth?.last_refresh === 'string' &&
      typeof tokens?.account_id === 'string' &&
      tokens.account_id.trim().length > 0 &&
      tokens.refresh_token === '' &&
      isSyntheticJwt(tokens.access_token) &&
      isSyntheticJwt(tokens.id_token)
    );
  } catch {
    return false;
  }
}

function atomicJsonWrite(file, document) {
  const temporary = file + '.' + process.pid + '.tmp';
  fs.writeFileSync(temporary, JSON.stringify(document), { mode: 0o600 });
  fs.renameSync(temporary, file);
}

function metadataPath(directory, id) {
  return path.join(directory, id + '.json');
}

function turnKey(agent, sessionKey) {
  return `${agent}:${sessionKey}`;
}

function validSessionKey(value) {
  return typeof value === 'string' && /^[A-Za-z0-9._:-]{1,256}$/.test(value);
}

function loadJobs(directory, kind) {
  for (const name of fs.readdirSync(directory)) {
    if (!name.endsWith('.json')) continue;
    try {
      const record = JSON.parse(fs.readFileSync(path.join(directory, name), 'utf8'));
      if (!record || typeof record.id !== 'string') continue;
      if (record.status === 'running') {
        record.status = 'interrupted';
        record.exit_code = null;
        record.blocking = kind === 'turn';
        record.updated_at = new Date().toISOString();
        atomicJsonWrite(path.join(directory, name), record);
      }
      jobs.set(record.id, { ...record, kind });
      if (kind === 'turn' && record.agent && validSessionKey(record.session_key)) {
        const key = turnKey(record.agent, record.session_key);
        const previous = latestTurns.get(key);
        if (!previous || String(record.created_at) > String(previous.created_at)) {
          latestTurns.set(key, record);
        }
        if (record.blocking) activeTurns.set(key, record.id);
      }
    } catch {
      // Ignore an incomplete or corrupt record; it cannot safely be resumed.
    }
  }
}

loadJobs(RUN_DIR, 'run');
loadJobs(TURN_DIR, 'turn');

function createJob(kind, fields) {
  const id = crypto.randomUUID();
  const directory = kind === 'run' ? RUN_DIR : TURN_DIR;
  const job = {
    id,
    kind,
    status: 'running',
    exit_code: null,
    created_at: new Date().toISOString(),
    updated_at: new Date().toISOString(),
    truncated: false,
    stored_bytes: 0,
    ...fields
  };
  jobs.set(id, job);
  atomicJsonWrite(metadataPath(directory, id), publicMetadata(job));
  return job;
}

function publicMetadata(job) {
  const {
    id,
    kind,
    status,
    exit_code,
    created_at,
    updated_at,
    truncated,
    agent,
    session_key,
    native_session_id,
    blocking
  } = job;
  return {
    id,
    kind,
    status,
    exit_code,
    created_at,
    updated_at,
    truncated,
    agent,
    session_key,
    native_session_id,
    blocking
  };
}

function saveJob(job) {
  job.updated_at = new Date().toISOString();
  const directory = job.kind === 'run' ? RUN_DIR : TURN_DIR;
  atomicJsonWrite(metadataPath(directory, job.id), publicMetadata(job));
}

function appendBounded(job, filename, chunk) {
  const remaining = MAX_JOB_OUTPUT_BYTES - job.stored_bytes;
  if (remaining <= 0) {
    job.truncated = true;
    return;
  }
  const bytes = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk);
  const stored = bytes.length > remaining ? bytes.subarray(0, remaining) : bytes;
  fs.appendFileSync(filename, stored, { mode: 0o600 });
  job.stored_bytes += stored.length;
  if (stored.length < bytes.length) job.truncated = true;
}

function readBounded(filename, limit = MAX_RESPONSE_OUTPUT_BYTES) {
  try {
    const fd = fs.openSync(filename, 'r');
    try {
      const size = fs.fstatSync(fd).size;
      const length = Math.min(size, limit);
      const buffer = Buffer.alloc(length);
      fs.readSync(fd, buffer, 0, length, Math.max(0, size - length));
      return { text: buffer.toString('utf8'), truncated: size > limit };
    } finally {
      fs.closeSync(fd);
    }
  } catch (err) {
    if (err.code === 'ENOENT') return { text: '', truncated: false };
    throw err;
  }
}

function runOutputPath(job) {
  return path.join(RUN_DIR, job.id + '.output');
}

function turnEventsPath(job) {
  return path.join(TURN_DIR, job.id + '.events.jsonl');
}

function turnStderrPath(job) {
  return path.join(TURN_DIR, job.id + '.stderr');
}

function killProcessGroup(child, signal) {
  if (!child.pid) return;
  try {
    process.kill(-child.pid, signal);
  } catch (err) {
    if (err.code !== 'ESRCH') child.kill(signal);
  }
}

function runChild(job, child, timeoutMs, onStart) {
  let timedOut = false;
  let spawnError = null;
  let finalized = false;
  let exitCode = null;
  const finish = (code) => {
    if (finalized) return;
    finalized = true;
    if (timedOut) job.status = 'timed_out';
    else if (job.stop_requested) job.status = 'interrupted';
    else if (spawnError) job.status = 'failed';
    else job.status = code === 0 ? 'completed' : 'failed';
    job.exit_code = code;
    job.blocking = false;
    if (spawnError) job.error = 'process could not start';
    saveJob(job);
    const key = turnKey(job.agent, job.session_key);
    if (job.kind === 'turn' && activeTurns.get(key) === job.id) {
      activeTurns.delete(key);
    }
    if (job.kind === 'turn') turnProcesses.delete(job.id);
    if (job.finishTurn) job.finishTurn();
  };
  const timer = setTimeout(() => {
    timedOut = true;
    job.timed_out = true;
    killProcessGroup(child, 'SIGTERM');
    const killTimer = setTimeout(() => {
      killProcessGroup(child, 'SIGKILL');
      finish(exitCode);
    }, 500);
    killTimer.unref();
  }, timeoutMs);
  timer.unref();
  child.once('error', (err) => {
    spawnError = err;
  });
  onStart(child);
  child.once('close', (code) => {
    clearTimeout(timer);
    exitCode = code;
    if (!timedOut) finish(code);
  });
}

function parseTurn(job) {
  const file = turnEventsPath(job);
  const content = readBounded(file, MAX_JOB_OUTPUT_BYTES).text;
  const lines = content.split('\n');
  const parsed = [];
  let nativeSessionId = job.native_session_id || null;
  let finalMessage = null;
  for (const line of lines) {
    if (!line.trim()) continue;
    let event;
    try {
      event = JSON.parse(line);
    } catch {
      continue;
    }
    if (!event || typeof event !== 'object' || Array.isArray(event)) continue;
    if (event.session_id && job.agent === 'claude') nativeSessionId = event.session_id;
    if (event.thread_id && job.agent === 'codex') nativeSessionId = event.thread_id;
    if (event.type === 'thread.started' && event.thread_id) nativeSessionId = event.thread_id;
    if (event.type === 'result' && typeof event.result === 'string') finalMessage = event.result;
    const item = event.item || (event.params && event.params.item);
    if (
      job.agent === 'codex' &&
      item &&
      (item.type === 'agent_message' || item.type === 'agentMessage') &&
      typeof item.text === 'string'
    )
      finalMessage = item.text;
    parsed.push(event);
  }
  return {
    native_session_id: nativeSessionId,
    final_message: finalMessage,
    events: parsed.slice(-MAX_RETURN_EVENTS)
  };
}

function turnResponse(job) {
  const parsed = parseTurn(job);
  if (parsed.native_session_id !== job.native_session_id) {
    job.native_session_id = parsed.native_session_id;
    saveJob(job);
  }
  const stderr = readBounded(turnStderrPath(job));
  const authDiagnostic = `${stderr.text}\n${JSON.stringify(parsed.events)}`;
  const credentialRejected =
    job.status === 'failed' &&
    /(?:HTTP\s+)?401\b|unauthori[sz]ed|invalid[_ ]api[_ ]key|authentication_failed/i.test(
      authDiagnostic
    );
  return {
    id: job.id,
    agent: job.agent,
    status: job.status,
    exit_code: job.exit_code,
    native_session_id: parsed.native_session_id,
    final_message: parsed.final_message,
    events: parsed.events,
    stderr: stderr.text,
    credential_rejected: credentialRejected,
    truncated: job.truncated || stderr.truncated
  };
}

function findJournal(agent, id) {
  const root =
    agent === 'claude'
      ? path.join(process.env.CLAUDE_CONFIG_DIR || path.join(HOME_PATH, '.claude'), 'projects')
      : path.join(CODEX_HOME_PATH, 'sessions');
  const expected = agent === 'claude' ? `${id}.jsonl` : null;
  const stack = [root];
  let visited = 0;
  while (stack.length > 0 && visited < MAX_JOURNAL_SEARCH_ENTRIES) {
    const directory = stack.pop();
    let entries;
    try {
      entries = fs.readdirSync(directory, { withFileTypes: true });
    } catch (err) {
      if (err.code === 'ENOENT' || err.code === 'ENOTDIR' || err.code === 'EACCES') continue;
      throw err;
    }
    for (const entry of entries) {
      visited += 1;
      if (visited > MAX_JOURNAL_SEARCH_ENTRIES) break;
      const candidate = path.join(directory, entry.name);
      if (entry.isDirectory()) stack.push(candidate);
      else if (
        entry.isFile() &&
        (agent === 'claude'
          ? entry.name === expected
          : entry.name.startsWith('rollout-') && entry.name.endsWith(`-${id}.jsonl`))
      )
        return candidate;
    }
  }
  return null;
}

async function journalPage(file, from, limit) {
  const lines = [];
  let totalLines = 0;
  let responseBytes = 0;
  let pending = Buffer.alloc(0);
  const stream = fs.createReadStream(file);
  for await (const chunk of stream) {
    let offset = 0;
    let boundary;
    while ((boundary = chunk.indexOf(0x0a, offset)) !== -1) {
      const part = chunk.subarray(offset, boundary);
      const line = pending.length ? Buffer.concat([pending, part]) : part;
      pending = Buffer.alloc(0);
      if (line.length > MAX_JOURNAL_LINE_BYTES) throw new RangeError('journal line too large');
      totalLines += 1;
      const cost = line.length + 24;
      if (
        totalLines > from &&
        totalLines <= from + limit &&
        responseBytes + cost <= MAX_JOURNAL_RESPONSE_BYTES
      ) {
        lines.push({ line: totalLines, text: line.toString('utf8') });
        responseBytes += cost;
      }
      offset = boundary + 1;
    }
    if (offset < chunk.length) {
      const rest = chunk.subarray(offset);
      pending = pending.length ? Buffer.concat([pending, rest]) : rest;
      if (pending.length > MAX_JOURNAL_LINE_BYTES) throw new RangeError('journal line too large');
    }
  }
  return { file, total_lines: totalLines, lines };
}

async function getJournal(req, res) {
  const query = new URL(req.url, 'http://exec-shim.invalid').searchParams;
  const agent = query.get('agent');
  const id = query.get('id');
  const fromText = query.get('from') || '0';
  const limitText = query.get('limit') || String(MAX_JOURNAL_LINES);
  if (
    (agent !== 'claude' && agent !== 'codex') ||
    typeof id !== 'string' ||
    !/^[A-Za-z0-9._:-]{1,256}$/.test(id) ||
    !/^\d{1,12}$/.test(fromText) ||
    !/^\d{1,4}$/.test(limitText)
  ) {
    res.writeHead(400).end('invalid journal query');
    return;
  }
  const from = Number(fromText);
  const limit = Number(limitText);
  if (
    !Number.isSafeInteger(from) ||
    !Number.isInteger(limit) ||
    limit < 1 ||
    limit > MAX_JOURNAL_LINES
  ) {
    res.writeHead(400).end('invalid journal page');
    return;
  }
  const file = findJournal(agent, id);
  if (!file) {
    json(res, 200, { file: null, total_lines: 0, lines: [] });
    return;
  }
  try {
    json(res, 200, await journalPage(file, from, limit));
  } catch (err) {
    if (err instanceof RangeError) {
      res.writeHead(413).end('journal line too large');
      return;
    }
    res.writeHead(500).end('journal could not be read');
  }
}

function workspaceReady() {
  try {
    const stat = fs.statSync(WORKSPACE_PATH);
    if (!stat.isDirectory()) return false;
    fs.accessSync(WORKSPACE_PATH, fs.constants.R_OK | fs.constants.W_OK);
    return true;
  } catch {
    return false;
  }
}

function validateTimeout(value, fallback) {
  if (value === undefined) return fallback;
  if (!Number.isInteger(value) || value < 1 || value > MAX_TIMEOUT_MS) return null;
  return value;
}

function handleBody(req, res, callback, maxBytes = MAX_REQUEST_BODY_BYTES) {
  requestBody(
    req,
    res,
    (body) => {
      let document;
      try {
        document = JSON.parse(body);
      } catch {
        res.writeHead(400).end('invalid json');
        return;
      }
      if (!document || typeof document !== 'object' || Array.isArray(document)) {
        res.writeHead(400).end('invalid request');
        return;
      }
      callback(document);
    },
    maxBytes
  );
}

function startRun(document, res) {
  const command = document.command;
  const timeoutMs = validateTimeout(document.timeout_ms, DEFAULT_RUN_TIMEOUT_MS);
  if (typeof command !== 'string' || command.length === 0) {
    res.writeHead(400).end('missing command');
    return;
  }
  if (timeoutMs === null) {
    res.writeHead(400).end('invalid timeout_ms');
    return;
  }
  if (!workspaceReady()) {
    res.writeHead(503).end('workspace is not ready');
    return;
  }
  const job = createJob('run', { blocking: false });
  try {
    const child = spawn('/bin/sh', ['-lc', command], {
      cwd: WORKSPACE_PATH,
      env: process.env,
      stdio: ['ignore', 'pipe', 'pipe'],
      detached: true
    });
    runChild(job, child, timeoutMs, (processChild) => {
      processChild.stdout.on('data', (chunk) => appendBounded(job, runOutputPath(job), chunk));
      processChild.stderr.on('data', (chunk) => appendBounded(job, runOutputPath(job), chunk));
    });
    json(res, 202, { id: job.id, status: job.status });
  } catch {
    job.status = 'failed';
    job.error = 'process could not start';
    saveJob(job);
    json(res, 500, { id: job.id, status: job.status });
  }
}

function startTurn(document, res) {
  const agent = document.agent;
  const prompt = document.prompt;
  const sessionId = document.session_id;
  const sessionKey = document.session_key;
  const resume = document.resume;
  const timeoutMs = validateTimeout(document.timeout_ms, DEFAULT_TURN_TIMEOUT_MS);
  if (agent !== 'claude' && agent !== 'codex') {
    res.writeHead(400).end('unsupported agent');
    return;
  }
  if (typeof prompt !== 'string' || prompt.length === 0) {
    res.writeHead(400).end('missing prompt');
    return;
  }
  if (!validSessionKey(sessionKey)) {
    res.writeHead(400).end('invalid session_key');
    return;
  }
  if (typeof resume !== 'boolean') {
    res.writeHead(400).end('invalid resume intent');
    return;
  }
  if (
    sessionId !== undefined &&
    (typeof sessionId !== 'string' || !/^[A-Za-z0-9._:-]{1,256}$/.test(sessionId))
  ) {
    res.writeHead(400).end('invalid session_id');
    return;
  }
  if (resume && !sessionId) {
    res.writeHead(400).end('resume requires an established session_id');
    return;
  }
  if (!resume && agent === 'claude' && !sessionId) {
    res.writeHead(400).end('new Claude sessions require a session_id');
    return;
  }
  if (timeoutMs === null) {
    res.writeHead(400).end('invalid timeout_ms');
    return;
  }
  if (!workspaceReady()) {
    res.writeHead(503).end('workspace is not ready');
    return;
  }
  const key = turnKey(agent, sessionKey);
  if (activeTurns.has(key)) {
    res.writeHead(409).end('turn already in flight for session');
    return;
  }

  const job = createJob('turn', {
    agent,
    session_key: sessionKey,
    native_session_id: sessionId || null,
    resume,
    blocking: true
  });
  job.completion = new Promise((resolve) => {
    job.finishTurn = resolve;
  });
  activeTurns.set(key, job.id);
  latestTurns.set(key, job);
  try {
    const child = spawn(
      LAUNCHER,
      [agent, resume ? 'resume' : 'create', ...(sessionId ? [sessionId] : [])],
      {
        cwd: WORKSPACE_PATH,
        env: process.env,
        stdio: ['pipe', 'pipe', 'pipe'],
        detached: true
      }
    );
    runChild(job, child, timeoutMs, (processChild) => {
      turnProcesses.set(job.id, processChild);
      processChild.stdout.on('data', (chunk) => appendBounded(job, turnEventsPath(job), chunk));
      processChild.stderr.on('data', (chunk) => appendBounded(job, turnStderrPath(job), chunk));
      processChild.stdin.on('error', () => {});
      processChild.stdin.end(prompt, 'utf8');
    });
    json(res, 202, { id: job.id, status: job.status });
  } catch {
    activeTurns.delete(key);
    job.status = 'failed';
    job.blocking = false;
    job.error = 'process could not start';
    saveJob(job);
    json(res, 500, { id: job.id, status: job.status });
  }
}

function currentTurn(agent, sessionKey, res) {
  if (agent !== 'claude' && agent !== 'codex') {
    res.writeHead(400).end('unsupported agent');
    return;
  }
  if (!validSessionKey(sessionKey)) {
    res.writeHead(400).end('invalid session_key');
    return;
  }
  const job = latestTurns.get(turnKey(agent, sessionKey));
  if (!job) {
    res.writeHead(404).end('no turn');
    return;
  }
  json(res, 200, turnResponse(job));
}

function agentReady(agent, res) {
  if (agent !== 'claude' && agent !== 'codex') {
    res.writeHead(400).end('unsupported agent');
    return;
  }
  const credential = credentialPaths.get(agent === 'claude' ? 'claude-token' : 'codex-auth');
  try {
    const stat = fs.statSync(credential);
    if (stat.isFile() && stat.size > 0) {
      json(res, 200, { agent, configured: true });
      return;
    }
  } catch {
    // A missing credential is a readiness failure; never include file contents or paths.
  }
  res.writeHead(503).end('agent credential unavailable');
}

async function stopTurn(document, res) {
  const agent = document.agent;
  const sessionKey = document.session_key;
  if (agent !== 'claude' && agent !== 'codex') {
    res.writeHead(400).end('unsupported agent');
    return;
  }
  if (!validSessionKey(sessionKey)) {
    res.writeHead(400).end('invalid session_key');
    return;
  }
  const key = turnKey(agent, sessionKey);
  const id = activeTurns.get(key);
  const child = id && turnProcesses.get(id);
  const job = id && jobs.get(id);
  if (!job) {
    json(res, 200, { status: 'not_running' });
    return;
  }
  if (!child) {
    // A restored actor can retain an interrupted job record without a live process. The caller
    // explicitly requested stop, so release the per-agent turn lock without replaying anything.
    job.status = 'interrupted';
    job.blocking = false;
    saveJob(job);
    activeTurns.delete(key);
    json(res, 200, { id, status: job.status });
    return;
  }
  job.stop_requested = true;
  saveJob(job);
  killProcessGroup(child, 'SIGTERM');
  const killTimer = setTimeout(() => killProcessGroup(child, 'SIGKILL'), 500);
  killTimer.unref();
  await job.completion;
  clearTimeout(killTimer);
  json(res, 200, { id, status: job.status });
}

function getJob(kind, id, res) {
  if (!/^[0-9a-f-]{36}$/i.test(id)) {
    res.writeHead(404).end('not found');
    return;
  }
  const job = jobs.get(id);
  if (!job || job.kind !== kind) {
    res.writeHead(404).end('not found');
    return;
  }
  if (kind === 'turn') {
    json(res, 200, turnResponse(job));
    return;
  }
  const output = readBounded(runOutputPath(job));
  json(res, 200, {
    id: job.id,
    status: job.status,
    exit_code: job.exit_code,
    output: output.text,
    truncated: job.truncated || output.truncated
  });
}

function processSummary(entry) {
  return {
    id: entry.id,
    label: entry.label,
    status: entry.status,
    started_at: entry.started_at,
    exit_code: entry.exit_code,
    log_tail: entry.log.toString('utf8')
  };
}

function listProcesses(res) {
  json(res, 200, { processes: [...longProcesses.values()].map(processSummary) });
}

function startLongProcess(document, res) {
  const command = document.command;
  const label = document.label === undefined ? 'process' : document.label;
  const running = [...longProcesses.values()].filter((entry) => entry.status === 'running');
  if (
    typeof command !== 'string' ||
    !command.trim() ||
    Buffer.byteLength(command, 'utf8') > 8192 ||
    typeof label !== 'string' ||
    !label.trim() ||
    label.length > 100
  ) {
    res.writeHead(400).end('invalid process');
    return;
  }
  if (running.length >= MAX_LONG_PROCESSES) {
    res.writeHead(409).end('process limit reached');
    return;
  }

  const id = crypto.randomUUID();
  let child;
  try {
    child = spawn('/bin/sh', ['-lc', command], {
      cwd: WORKSPACE_PATH,
      env: process.env,
      detached: true,
      stdio: ['ignore', 'pipe', 'pipe']
    });
  } catch {
    res.writeHead(500).end('process could not start');
    return;
  }
  const entry = {
    id,
    label: label.trim(),
    status: 'running',
    started_at: new Date().toISOString(),
    exit_code: null,
    log: Buffer.alloc(0),
    child,
    completion: null
  };
  const appendLog = (chunk) => {
    const combined = Buffer.concat([entry.log, chunk]);
    entry.log = combined.subarray(Math.max(0, combined.length - MAX_LONG_PROCESS_LOG_BYTES));
  };
  child.stdout.on('data', appendLog);
  child.stderr.on('data', appendLog);
  entry.completion = new Promise((resolve) => {
    child.once('error', () => {
      entry.status = 'failed';
      entry.exit_code = null;
      resolve();
    });
    child.once('exit', (code, signal) => {
      entry.status = signal ? 'stopped' : code === 0 ? 'completed' : 'failed';
      entry.exit_code = Number.isInteger(code) ? code : null;
      resolve();
    });
  });
  longProcesses.set(id, entry);
  while (longProcesses.size > MAX_LONG_PROCESS_RECORDS) {
    const oldest = longProcesses.values().next().value;
    if (oldest.status === 'running') break;
    longProcesses.delete(oldest.id);
  }
  json(res, 201, { id, status: entry.status });
}

async function stopLongProcess(id, res) {
  if (!/^[0-9a-f-]{36}$/i.test(id)) {
    res.writeHead(404).end('not found');
    return;
  }
  const entry = longProcesses.get(id);
  if (!entry) {
    res.writeHead(404).end('not found');
    return;
  }
  if (entry.status === 'running') {
    killProcessGroup(entry.child, 'SIGTERM');
    const killTimer = setTimeout(() => killProcessGroup(entry.child, 'SIGKILL'), 500);
    killTimer.unref();
    await entry.completion;
    clearTimeout(killTimer);
  }
  json(res, 200, { id, status: entry.status });
}

function listeningPorts() {
  const ports = new Set();
  for (const name of ['tcp', 'tcp6']) {
    let contents;
    try {
      contents = fs.readFileSync(path.join(PROC_NET_DIR, name), 'utf8');
    } catch {
      continue;
    }
    for (const line of contents.split('\n').slice(1)) {
      const columns = line.trim().split(/\s+/);
      if (columns.length < 4 || columns[3] !== '0A') continue;
      const local = columns[1].split(':');
      if (local.length !== 2) continue;
      const port = Number.parseInt(local[1], 16);
      if (Number.isInteger(port) && port > 0 && port <= 65535) ports.add(port);
    }
  }
  return [...ports].sort((left, right) => left - right);
}

const server = http.createServer((req, res) => {
  if (req.method === 'GET' && (req.url === '/healthz' || req.url === '/readyz')) {
    if (!workspaceReady()) {
      res.writeHead(503, { 'content-type': 'text/plain' }).end('workspace not ready');
      return;
    }
    res.writeHead(200, { 'content-type': 'text/plain' }).end('ok');
    return;
  }
  if (req.method === 'POST' && req.url === '/token') {
    if (bearerToken !== null) {
      res.writeHead(409).end('token already set');
      return;
    }
    handleBody(req, res, (document) => {
      try {
        const installed = installToken(document.token);
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
  if ((req.method === 'POST' || req.method === 'PUT') && req.url === '/credential') {
    if (!authorized(req)) return unauthorized(res);
    const replace = req.method === 'PUT';
    handleBody(
      req,
      res,
      (document) => {
        try {
          const installed = installCredential(document.name, document.contents, replace);
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
          res.writeHead(replace ? 200 : 201).end('credential stored');
        } catch {
          res.writeHead(500).end('credential could not be stored');
        }
      },
      MAX_CREDENTIAL_REQUEST_BODY_BYTES
    );
    return;
  }
  if (req.method === 'POST' && req.url === '/run') {
    if (!authorized(req)) return unauthorized(res);
    handleBody(req, res, (document) => startRun(document, res));
    return;
  }
  if (req.method === 'POST' && req.url === '/processes') {
    if (!authorized(req)) return unauthorized(res);
    handleBody(req, res, (document) => startLongProcess(document, res));
    return;
  }
  if (req.method === 'GET' && req.url === '/processes') {
    if (!authorized(req)) return unauthorized(res);
    listProcesses(res);
    return;
  }
  if (req.method === 'GET' && req.url === '/ports') {
    if (!authorized(req)) return unauthorized(res);
    json(res, 200, { ports: listeningPorts() });
    return;
  }
  const processMatch = req.url.match(/^\/processes\/([^/?]+)$/);
  if (req.method === 'DELETE' && processMatch) {
    if (!authorized(req)) return unauthorized(res);
    void stopLongProcess(processMatch[1], res);
    return;
  }
  if (req.method === 'POST' && req.url === '/turn') {
    if (!authorized(req)) return unauthorized(res);
    handleBody(req, res, (document) => startTurn(document, res));
    return;
  }
  if (req.method === 'POST' && req.url === '/turn/stop') {
    if (!authorized(req)) return unauthorized(res);
    handleBody(req, res, (document) => void stopTurn(document, res));
    return;
  }
  if (req.method === 'GET' && req.url.startsWith('/turn/status?')) {
    if (!authorized(req)) return unauthorized(res);
    const query = new URL(req.url, 'http://exec-shim.invalid').searchParams;
    currentTurn(query.get('agent'), query.get('session_key'), res);
    return;
  }
  if (req.method === 'GET' && req.url.startsWith('/agent/ready?')) {
    if (!authorized(req)) return unauthorized(res);
    const query = new URL(req.url, 'http://exec-shim.invalid').searchParams;
    agentReady(query.get('agent'), res);
    return;
  }
  if (req.method === 'GET' && req.url.startsWith('/journal?')) {
    if (!authorized(req)) return unauthorized(res);
    void getJournal(req, res);
    return;
  }
  const match = req.method === 'GET' && req.url.match(/^\/(run|turn)\/([^/?]+)$/);
  if (match) {
    if (!authorized(req)) return unauthorized(res);
    getJob(match[1] === 'run' ? 'run' : 'turn', match[2], res);
    return;
  }
  res.writeHead(404).end();
});

server.listen(
  Number(process.env.EXEC_SHIM_PORT || 8090),
  process.env.EXEC_SHIM_HOST || '0.0.0.0',
  () => {
    const address = server.address();
    console.log('exec-shim listening on :' + address.port);
  }
);
