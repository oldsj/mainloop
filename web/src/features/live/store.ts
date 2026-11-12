/**
 * The live session view model. It folds the runner's Client event protocol (subprotocol
 * `kagent.sdk-runner.v1`, see the SDK runner contract) into a list of entries the view renders the
 * way Claude Code does: assistant text after a `●`, tool calls with their first output line after
 * `⎿`, edits as diffs, permission prompts with answer buttons, and queued user messages.
 *
 * Only prompts the runner holds live arrive as `perm` frames: a client's own turn, and prompts
 * outside the harness's approval boundary (the profile's own permission mode). A protected tool call
 * or question in an A2A turn is deferred instead (kagent #90): the turn ends with a `tool_deferred`
 * result and the request is answered through the A2A task, which the page shows beside this log.
 *
 * SDK messages arrive unchanged (`k: "sdk"`), so this reads the Agent SDK's message shapes
 * loosely: unknown frames and fields are ignored, as the protocol requires.
 */

export const MODE_IDS = ['default', 'acceptEdits', 'plan', 'auto'] as const;
export const MODE_LABELS: Record<string, string> = {
  default: 'default',
  acceptEdits: 'accept edits',
  plan: 'plan mode',
  auto: 'auto mode',
  bypassPermissions: 'bypass permissions'
};
export const EFFORTS = ['low', 'medium', 'high', 'xhigh', 'max'];

export type Status = 'idle' | 'running' | 'waiting';
export type ToolStatus = 'run' | 'ok' | 'err';
export type DiffLine = ['+' | '-', string];

export type Entry =
  | {
      t: 'user';
      id: number;
      text: string;
      uuid?: string;
      /** Mainloop's message id: set on a message the owner sent, and on the runner's echo of it. */
      mid?: string;
      queued: boolean;
      /** Mainloop recorded it to join the running turn, as a message sent mid-turn does. */
      joins?: boolean;
      origin?: string;
    }
  | { t: 'text'; id: number; text: string }
  | { t: 'sys'; id: number; text: string }
  | { t: 'tool'; id: number; name: string; arg: string; out: string[]; status: ToolStatus }
  | { t: 'diff'; id: number; verb: string; file: string; lines: DiffLine[] }
  | { t: 'perm'; id: number; permId: string }
  | { t: 'interrupted'; id: number };

/** An entry before it gets its id. */
type NewEntry = Entry extends infer E ? (E extends Entry ? Omit<E, 'id'> : never) : never;

/** An answer index: 0 yes, 1 yes and don't ask again, 2 no, -1 cancelled. */
export interface Perm {
  id: string;
  tool: string;
  arg: string;
  why: string;
  title: string;
  /** The SDK's tool_use_id for the call, which pairs the prompt with the session's request. */
  callId?: string;
  chosen: number | null;
}

export interface ModelOption {
  value: string;
  displayName?: string;
  description?: string;
  supportedEffortLevels?: string[];
}

export interface Command {
  name: string;
  description: string;
}

export type Frame = { k: string; seq?: number; ts?: number } & Record<string, unknown>;

/** One message of Mainloop's record of the conversation, which outlives the runner process. */
export interface HistoryMessage {
  role: 'user' | 'assistant';
  content: string;
}

type Obj = Record<string, unknown>;
const isObj = (v: unknown): v is Obj => typeof v === 'object' && v !== null && !Array.isArray(v);
const str = (v: unknown): string => (typeof v === 'string' ? v : '');

const base = (p: unknown) => str(p).replace(/^\/data\/workspace\//, '');

/** The one-line argument Claude Code shows after a tool's name. */
export function argOf(name: string, input: unknown): string {
  if (!isObj(input)) return '';
  if (name === 'Bash') return str(input.command);
  if (input.file_path) return base(input.file_path);
  if (input.pattern) return `"${str(input.pattern)}"${input.path ? ' ' + base(input.path) : ''}`;
  for (const k of ['url', 'query', 'description', 'prompt']) if (input[k]) return str(input[k]);
  const s = JSON.stringify(input);
  return s.length > 160 ? s.slice(0, 160) + '…' : s;
}

const linesOf = (s: string) => s.split('\n').slice(0, 400);

function diffOf(name: string, input: Obj): { verb: string; file: string; lines: DiffLine[] } {
  if (name === 'Write') {
    return {
      verb: 'Write',
      file: base(input.file_path),
      lines: linesOf(str(input.content))
        .slice(0, 60)
        .map((l): DiffLine => ['+', l])
    };
  }
  const edits = name === 'MultiEdit' && Array.isArray(input.edits) ? input.edits : [input];
  const lines: DiffLine[] = [];
  for (const e of edits) {
    if (!isObj(e)) continue;
    linesOf(str(e.old_string)).forEach((l) => lines.push(['-', l]));
    linesOf(str(e.new_string)).forEach((l) => lines.push(['+', l]));
  }
  return { verb: 'Update', file: base(input.file_path), lines: lines.slice(0, 80) };
}

function resultText(c: unknown): string {
  if (typeof c === 'string') return c;
  if (Array.isArray(c)) {
    return c
      .map((b) =>
        isObj(b) && b.type === 'text' ? str(b.text) : `[${isObj(b) ? str(b.type) : ''}]`
      )
      .join('\n');
  }
  return '';
}

export interface LiveState {
  entries: Entry[];
  perms: Record<string, Perm>;
  status: Status;
  /**
   * The last turn ended on a deferred tool call (`tool_deferred`): an approval or question in an
   * A2A turn now waits on the A2A task, not on this channel, and the runner refuses new messages
   * until it is answered. The view shows the session's requests to answer.
   */
  deferred: boolean;
  activity: string | null;
  model: string;
  effort: string;
  mode: string;
  cwd: string;
  sessionId: string;
  version: string;
  profile: string;
  runner: string;
  models: ModelOption[];
  commands: Command[];
}

export function initialState(): LiveState {
  return {
    entries: [],
    perms: {},
    status: 'idle',
    deferred: false,
    activity: null,
    model: '',
    effort: '',
    mode: 'default',
    cwd: '',
    sessionId: '',
    version: '',
    profile: '',
    runner: '',
    models: [],
    commands: []
  };
}

/**
 * A mutable reducer over frames. `apply` returns nothing; the owner re-renders from `state` after
 * a batch. Entries are replaced, not mutated, when they change, so a memoised row re-renders.
 */
export class LiveModel {
  state: LiveState = initialState();
  private nextId = 1;
  private tools = new Map<string, number>(); // tool_use id -> entry id
  private cur = new Map<number, number>(); // content block index -> text entry id
  private streamed = new Set<string>(); // assistant message ids seen as stream events
  private queued = new Map<string, number>(); // user uuid -> entry id
  // Messages the owner sent through Mainloop that the runner has not echoed yet, oldest first:
  // message id -> its text and entry. They survive a reattach, which rebuilds the entries.
  private sent = new Map<string, { text: string; id: number }>();
  private history: HistoryMessage[] = [];
  private historyIds = new Set<number>(); // entries placed from the history

  reset() {
    this.state = initialState();
    this.tools.clear();
    this.cur.clear();
    this.streamed.clear();
    this.queued.clear();
    this.historyIds.clear();
  }

  /**
   * Seeds the transcript from Mainloop's record, which survives the actor sleeping: the runner's
   * backlog lives in its process and starts empty after a sleep or a replayed answer. It is placed
   * above the live entries, up to the turn the live backlog starts with.
   */
  setHistory(messages: readonly HistoryMessage[]) {
    this.history = messages.filter((m) => m.content.trim());
    this.placeHistory();
  }

  /**
   * Puts the history before the live entries. Where the backlog starts with a user message that the
   * record also holds, the record stops before its last copy, so a turn is not shown twice.
   */
  private placeHistory() {
    const live = this.state.entries.filter((e) => !this.historyIds.has(e.id));
    this.historyIds.clear();
    const first = live.find((e) => e.t === 'user');
    let past = this.history;
    if (first && first.t === 'user') {
      const text = first.text.trim();
      const at = past.findLastIndex((m) => m.role === 'user' && m.content.trim() === text);
      if (at >= 0) past = past.slice(0, at);
    }
    const placed: Entry[] = [];
    const add = (e: NewEntry) => {
      const id = this.nextId++;
      this.historyIds.add(id);
      placed.push({ ...e, id } as Entry);
    };
    if (past.length) add({ t: 'sys', text: "Earlier in this session, from Mainloop's record" });
    for (const m of past) {
      if (m.role === 'user') add({ t: 'user', text: m.content, queued: false, origin: 'history' });
      else add({ t: 'text', text: m.content });
    }
    if (past.length && live.length) add({ t: 'sys', text: 'Live' });
    this.state.entries = [...placed, ...live];
  }

  /** The first frame of every attach: it replaces everything the view held. */
  hello(f: Frame) {
    const keep = { models: this.state.models, commands: this.state.commands };
    this.reset();
    Object.assign(this.state, keep);
    if (isObj(f.meta)) this.meta(f.meta);
    const runner = f.runner;
    this.state.runner =
      typeof runner === 'string' ? runner : isObj(runner) ? str(runner.state) : '';
    if (Array.isArray(f.backlog)) for (const b of f.backlog) if (isObj(b)) this.apply(b as Frame);
    if (Array.isArray(f.perms)) {
      for (const p of f.perms) {
        if (isObj(p) && !this.state.perms[str(p.id)]) this.apply({ ...p, k: 'perm' } as Frame);
      }
    }
    for (const [mid, m] of this.sent) {
      m.id = this.push({ t: 'user', text: m.text, mid, queued: true, origin: 'client' });
    }
    this.placeHistory();
  }

  private push(e: NewEntry): number {
    const id = this.nextId++;
    this.state.entries = [...this.state.entries, { ...e, id } as Entry];
    return id;
  }

  private update(id: number, fn: (e: Entry) => Entry) {
    this.state.entries = this.state.entries.map((e) => (e.id === id ? fn(e) : e));
  }

  private sys(text: string) {
    this.push({ t: 'sys', text });
  }

  private unqueue(uuids: unknown, suffix = '') {
    if (!Array.isArray(uuids)) return;
    for (const u of uuids) {
      const id = this.queued.get(str(u));
      if (id === undefined) continue;
      this.queued.delete(str(u));
      this.update(id, (e) => (e.t === 'user' ? { ...e, queued: false, text: e.text + suffix } : e));
    }
  }

  private unqueueAll() {
    this.unqueue([...this.queued.keys()]);
  }

  meta(m: Obj) {
    const s = this.state;
    if (Array.isArray(m.models) && m.models.length) {
      s.models = m.models.filter(isObj).map((x) => ({
        value: str(x.value),
        displayName: str(x.displayName) || undefined,
        description: str(x.description) || undefined,
        supportedEffortLevels: Array.isArray(x.supportedEffortLevels)
          ? x.supportedEffortLevels.map(str)
          : undefined
      }));
    }
    if (Array.isArray(m.commands) && m.commands.length) {
      const hide = new Set(
        Array.isArray(m.terminal_slash_commands) ? m.terminal_slash_commands : []
      );
      s.commands = m.commands
        .filter(isObj)
        .filter((c) => !hide.has(c.name))
        .map((c) => ({ name: '/' + str(c.name), description: str(c.description) }));
    } else if (!s.commands.length && Array.isArray(m.slash_commands)) {
      s.commands = m.slash_commands.map((n) => ({ name: '/' + str(n), description: '' }));
    }
    if (m.model) s.model = str(m.model);
    if (m.effort) s.effort = str(m.effort);
    if (m.cwd) s.cwd = str(m.cwd);
    if (m.permissionMode) s.mode = str(m.permissionMode);
    if (m.sessionId) s.sessionId = str(m.sessionId);
    if (m.version) s.version = str(m.version);
    if (m.profile) s.profile = str(m.profile);
  }

  apply(f: Frame) {
    const s = this.state;
    switch (f.k) {
      case 'hello':
        this.hello(f);
        return;
      case 'meta':
        if (isObj(f.meta)) this.meta(f.meta);
        return;
      case 'runner':
        s.runner = str(f.state);
        if (f.state === 'exited') {
          this.sys(
            `Runner exited${f.code != null ? ` (${String(f.code)})` : ''}; it restarts on the next message.`
          );
          s.status = 'idle';
          s.activity = null;
        }
        return;
      case 'user': {
        const joins = this.echoed(str(f.mid), str(f.text));
        const queued = s.status !== 'idle';
        const id = this.push({
          joins: queued && joins ? true : undefined,
          t: 'user',
          text: str(f.text),
          uuid: str(f.uuid) || undefined,
          mid: str(f.mid) || undefined,
          queued,
          origin: str(f.origin) || undefined
        });
        if (queued && f.uuid) this.queued.set(str(f.uuid), id);
        if (s.status === 'idle') {
          s.status = 'running';
          s.activity = 'Working';
        }
        return;
      }
      case 'turn':
        if (f.state === 'running') {
          if (s.status === 'idle') s.status = 'running';
          s.deferred = false;
          this.unqueue(f.uuids);
        } else if (f.state === 'idle') {
          s.status = 'idle';
          s.activity = null;
          this.unqueue(f.uuids);
        }
        return;
      case 'perm': {
        const id = str(f.id);
        const tool = str(f.toolName);
        s.perms = {
          ...s.perms,
          [id]: {
            id,
            tool,
            arg: argOf(tool, f.input),
            why: str(f.decisionReason) || str(f.description) || str(f.blockedPath),
            title: str(f.title) || `Claude wants to use ${tool}`,
            callId: str(f.toolUseID) || undefined,
            chosen: null
          }
        };
        this.push({ t: 'perm', permId: id });
        s.status = 'waiting';
        s.activity = null;
        return;
      }
      case 'perm_done': {
        const id = str(f.id);
        const p = s.perms[id];
        if (p && p.chosen == null) {
          const chosen = f.aborted ? -1 : f.behavior === 'deny' ? 2 : f.always ? 1 : 0;
          s.perms = { ...s.perms, [id]: { ...p, chosen } };
        }
        if (s.status === 'waiting' && !Object.values(s.perms).some((x) => x.chosen == null)) {
          s.status = 'running';
        }
        return;
      }
      case 'ctl':
        if (f.what === 'interrupt') return;
        // The SDK driver sets the model and effort at the start of every A2A turn, so most acks
        // repeat the current value: only a change is printed. Empty means the default.
        if (f.what === 'model' || f.what === 'effort') {
          const key = f.what;
          const next = str(f[key]) || str(f.value);
          if (next !== (s[key] || '')) this.sys(`Set ${key} to ${next || 'default'}`);
          s[key] = next;
        } else if (f.what === 'mode') {
          s.mode = str(f.mode) || str(f.value) || s.mode;
        }
        return;
      case 'err':
        this.sys(`Error: ${str(f.text)}`);
        return;
      case 'sdk':
        if (isObj(f.m)) this.sdk(f.m);
        return;
    }
  }

  private sdk(m: Obj) {
    const s = this.state;
    if (m.type === 'stream_event') {
      if (m.parent_tool_use_id) return; // sub-agent partials show when their messages land
      const ev = isObj(m.event) ? m.event : {};
      this.unqueue(m.user_message_uuids);
      if (ev.type === 'message_start') {
        if (s.status === 'idle') s.status = 'running';
        this.cur.clear();
        const msg = isObj(ev.message) ? ev.message : {};
        if (msg.id) this.streamed.add(str(msg.id));
      } else if (ev.type === 'content_block_start') {
        const b = isObj(ev.content_block) ? ev.content_block : {};
        const index = Number(ev.index);
        if (b.type === 'text') {
          this.cur.set(index, this.push({ t: 'text', text: str(b.text) }));
          s.activity = null;
        } else if (b.type === 'thinking' || b.type === 'redacted_thinking') {
          s.activity = 'Thinking';
        } else if (b.type === 'tool_use' || b.type === 'server_tool_use') {
          this.tools.set(
            str(b.id),
            this.push({ t: 'tool', name: str(b.name), arg: '', out: [], status: 'run' })
          );
          s.activity = null;
        }
      } else if (ev.type === 'content_block_delta') {
        const d = isObj(ev.delta) ? ev.delta : {};
        const id = this.cur.get(Number(ev.index));
        if (d.type === 'text_delta' && id !== undefined) {
          this.update(id, (e) => (e.t === 'text' ? { ...e, text: e.text + str(d.text) } : e));
        }
      }
      return;
    }
    if (m.type === 'command_lifecycle') {
      if (m.state !== 'queued') {
        this.unqueue([m.command_uuid], m.state === 'cancelled' ? '  (cancelled)' : '');
      }
      return;
    }
    if (m.type === 'assistant') {
      const sub = Boolean(m.parent_tool_use_id);
      const msg = isObj(m.message) ? m.message : {};
      const streamed = this.streamed.has(str(msg.id));
      const content = Array.isArray(msg.content) ? msg.content : [];
      for (const b of content) {
        if (!isObj(b)) continue;
        if (b.type === 'text' && !streamed && !sub) this.push({ t: 'text', text: str(b.text) });
        if (b.type === 'tool_use') {
          const name = str(b.name);
          let id = this.tools.get(str(b.id));
          if (id === undefined) {
            id = this.push({ t: 'tool', name, arg: '', out: [], status: 'run' });
            this.tools.set(str(b.id), id);
          }
          const arg = argOf(name, b.input);
          this.update(id, (e) =>
            e.t === 'tool' ? { ...e, name: (sub ? '↳ ' : '') + name, arg } : e
          );
          if (/^(Edit|MultiEdit|Write)$/.test(name) && isObj(b.input)) {
            this.push({ t: 'diff', ...diffOf(name, b.input) });
          }
        }
      }
      return;
    }
    if (m.type === 'user') {
      const msg = isObj(m.message) ? m.message : {};
      if (Array.isArray(msg.content)) {
        for (const b of msg.content) {
          if (!isObj(b) || b.type !== 'tool_result') continue;
          const id = this.tools.get(str(b.tool_use_id));
          if (id === undefined) continue;
          const out = linesOf(resultText(b.content)).filter((l, k) => k || l.trim());
          this.update(id, (e) =>
            e.t === 'tool' ? { ...e, status: b.is_error ? 'err' : 'ok', out } : e
          );
          if (s.status === 'running') s.activity = 'Working';
        }
      }
      if (m.uuid) this.unqueue([m.uuid]);
      return;
    }
    if (m.type === 'result') {
      if (m.stop_reason === 'tool_deferred') {
        // A protected call in an A2A turn: kagent parks it and asks through the A2A task.
        this.sys('Waiting for your answer to the request below.');
        s.deferred = true;
        s.status = 'idle';
        s.activity = null;
        return;
      }
      // Claude Code prints nothing at turn end; only an interrupted or failed turn gets a line.
      if (m.subtype !== 'success' || m.is_error) this.push({ t: 'interrupted' });
      s.status = 'idle';
      s.activity = null;
      return;
    }
    if (m.type === 'system') {
      switch (m.subtype) {
        case 'init':
          if (m.model) s.model = str(m.model);
          if (m.cwd) s.cwd = str(m.cwd);
          if (m.permissionMode) s.mode = str(m.permissionMode);
          if (m.session_id) s.sessionId = str(m.session_id);
          if (m.claude_code_version) s.version = str(m.claude_code_version);
          if (!s.commands.length && Array.isArray(m.slash_commands)) {
            s.commands = m.slash_commands.map((n) => ({ name: '/' + str(n), description: '' }));
          }
          return;
        case 'session_state_changed':
          s.status =
            m.state === 'running' ? 'running' : m.state === 'requires_action' ? 'waiting' : 'idle';
          s.activity = m.state === 'running' ? (s.activity ?? 'Working') : null;
          if (m.state === 'idle') this.unqueueAll();
          return;
        case 'status':
          if (m.status === 'compacting') s.activity = 'Compacting';
          if (m.permissionMode) s.mode = str(m.permissionMode);
          return;
        case 'compact_boundary':
          this.sys('Conversation compacted');
          return;
        case 'local_command_output':
          this.sys(str(m.content));
          return;
        case 'api_retry': {
          const err = isObj(m.error) ? str(m.error.message) : str(m.error);
          this.sys(
            `API retry ${m.attempt != null ? String(m.attempt) : ''}${err ? ': ' + err : ''}`
          );
          return;
        }
        case 'notification':
          this.sys(str(m.message) || str(m.title) || 'Notification');
          return;
        case 'permission_denied':
          this.sys(`Denied: ${str(m.tool_name)} ${str(m.reason) || str(m.message)}`.trim());
          return;
        case 'informational':
          this.sys(str(m.content) || str(m.message));
          return;
      }
    }
  }

  /** Records the owner's answer before the runner confirms it, so the buttons cannot be pressed twice. */
  choose(permId: string, chosen: number) {
    const p = this.state.perms[permId];
    if (!p || p.chosen != null) return false;
    this.state.perms = { ...this.state.perms, [permId]: { ...p, chosen } };
    return true;
  }

  /** Takes back an answer that did not reach Mainloop, so it can be given again. */
  unchoose(permId: string) {
    const p = this.state.perms[permId];
    if (p) this.state.perms = { ...this.state.perms, [permId]: { ...p, chosen: null } };
  }

  note(text: string) {
    this.sys(text);
  }

  /**
   * Shows a message the owner sent through Mainloop at once. The runner's echo replaces it where the
   * runner puts it: matched by Mainloop's message id, or by text while the runner does not echo one.
   */
  sending(mid: string, text: string) {
    const queued = this.state.status !== 'idle';
    this.sent.set(mid, { text, id: this.push({ t: 'user', text, mid, queued, origin: 'client' }) });
  }

  /** Mainloop recorded the message to join the running turn, or behind it. */
  recorded(mid: string, joins: boolean) {
    // The runner's echo can come first; it carries the same id.
    this.state.entries = this.state.entries.map((e) =>
      e.t === 'user' && e.mid === mid && (e.queued || this.sent.get(mid)?.id === e.id)
        ? { ...e, queued: true, joins }
        : e
    );
  }

  /** Mainloop did not record the message: it stays shown, marked unsent. */
  notSent(mid: string, why: string) {
    const m = this.sent.get(mid);
    if (!m) return;
    this.sent.delete(mid);
    this.update(m.id, (e) => (e.t === 'user' ? { ...e, queued: false } : e));
    this.sys(`Not sent: ${why}`);
  }

  /** Messages sent through Mainloop that the runner has not echoed yet. */
  get unechoed(): number {
    return this.sent.size;
  }

  /** A model or effort Mainloop set for the session's next turns, shown as Claude Code does. */
  control(what: 'model' | 'effort', value: string) {
    if (what === 'model') this.state.model = value;
    else this.state.effort = value;
    this.sys(`Set ${what} to ${value}`);
  }

  /** Drops the shown copy of a message the runner echoed; whether it was recorded to join the turn. */
  private echoed(mid: string, text: string): boolean {
    let key = mid && this.sent.has(mid) ? mid : '';
    if (!key) {
      const t = text.trim();
      for (const [k, m] of this.sent) {
        if (m.text.trim() === t) {
          key = k;
          break;
        }
      }
    }
    const m = key ? this.sent.get(key) : undefined;
    if (!m) return false;
    this.sent.delete(key);
    const shown = this.state.entries.find((e) => e.id === m.id);
    this.state.entries = this.state.entries.filter((e) => e.id !== m.id);
    return shown?.t === 'user' && Boolean(shown.joins);
  }
}
