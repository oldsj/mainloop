import assert from 'node:assert/strict';
import { test } from 'node:test';
import { readFile, writeFile, mkdtemp, rm } from 'node:fs/promises';
import { resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import { compile } from 'svelte/compiler';
import { render } from 'svelte/server';
import {
  buildHITLResponse,
  createHITLChannel,
  parseHITL,
  reasonNotice,
  hitlStatus,
  type HITLView,
  type HITLState
} from './hitl.ts';

const payload = {
  type: 'tool_approval_request',
  tools: [
    { id: 'a', call_id: 'native-a', name: 'tool.a', args: { x: '<script>' } },
    { id: 'b', call_id: 'native-b', name: 'tool.b', args: {} }
  ],
  unknown: { retained: true }
};
const draft = () => ({
  decisions: { a: 'approve', b: 'reject' } as const,
  reasons: { b: 'Keep tests' },
  answers: []
});
const view = (id = 'request'): HITLView => ({
  request: {
    id,
    payload,
    availability: 'pending',
    outer: {
      runtime_session_id: 'session',
      task_id: 'task',
      context_id: 'context',
      request_hash: 'hash'
    }
  },
  response: null,
  route_request_id: id,
  answerable: true,
  writes_enabled: true,
  transport_state: null,
  unavailable_reason: null
});
const state = (v: HITLView): HITLState => ({
  view: v,
  busy: false,
  error: null,
  uncertain: false,
  stale: false
});

test('complete per-call choices, exact nested IDs, and no contradictory reasons', () => {
  assert.deepEqual(parseHITL(payload), payload);
  const input = draft();
  assert.deepEqual(buildHITLResponse(payload, input), {
    type: 'tool_approval_response',
    approvals: [
      { id: 'a', approved: true },
      { id: 'b', approved: false, rejection_reason: 'Keep tests' }
    ]
  });
  assert.throws(
    () => buildHITLResponse(payload, { ...input, decisions: { a: 'approve' } }),
    /every call/
  );
  assert.throws(
    () => buildHITLResponse(payload, { ...input, decisions: { a: 'approve', z: 'reject' } }),
    /every call/
  );
  assert.equal(parseHITL({ ...payload, tools: [payload.tools[0], payload.tools[0]] }), null);
  assert.equal(parseHITL({ type: 'future', tools: payload.tools }), null);
  assert.equal(parseHITL({ ...payload, tools: [{ id: 'x' }] }), null);
  assert.equal(parseHITL({ ...payload, nested: {} }), null);
  const nested = {
    ...payload,
    nested: { task_id: 'child-task', context_id: 'child-context', tools: [payload.tools[1]] }
  };
  assert.deepEqual(
    buildHITLResponse(nested, { decisions: { b: 'reject' }, reasons: {}, answers: [] }),
    { type: 'tool_approval_response', approvals: [{ id: 'b', approved: false }] }
  );
  const approval = buildHITLResponse(payload, { ...input, reasons: { a: 'old rejection', b: '' } });
  assert.equal(
    approval.type === 'tool_approval_response' && approval.approvals[0].rejection_reason,
    undefined
  );
});

test('questions with choices, multiple selection and native null free text', () => {
  const question = {
    type: 'ask_user_request',
    id: 'q',
    questions: [
      { question: 'Targets?', choices: ['a', 'b'], multiple: true },
      { question: 'Why?', choices: null, multiple: false }
    ]
  };
  const input = { decisions: {}, reasons: {}, answers: [['a', 'b'], ['Because']] };
  assert.deepEqual(buildHITLResponse(question, input), {
    type: 'ask_user_response',
    id: 'q',
    answers: [{ answer: ['a', 'b'] }, { answer: ['Because'] }]
  });
  assert.throws(
    () => buildHITLResponse(question, { ...input, answers: [['a']] }),
    /every question/
  );
  assert.throws(
    () => buildHITLResponse(question, { ...input, answers: [['c'], ['ok']] }),
    /offered/
  );
  assert.throws(
    () => buildHITLResponse(question, { ...input, answers: [['a'], ['one', 'two']] }),
    /one answer/
  );
  assert.throws(
    () => buildHITLResponse(question, { ...input, answers: [['a'], [' ']] }),
    /every question/
  );
  const nested = {
    ...question,
    nested: { task_id: 'child', context_id: 'ctx', tools: [payload.tools[0]] }
  };
  assert.equal((buildHITLResponse(nested, input) as { id: string }).id, 'a');
});

test('inbox/chat share a single flight; new requests do not inherit consent', async () => {
  const current = view();
  let sends = 0;
  let finish!: (v: HITLView) => void;
  const channel = createHITLChannel(
    async (id) => ({ ...current, request: { ...current.request, id }, route_request_id: id }),
    async () => {
      sends++;
      return new Promise((resolve) => (finish = resolve));
    },
    () => 'action'
  );
  let mobile = state(current),
    chat = state(current);
  channel.subscribe('request', (s) => (mobile = s));
  channel.subscribe('request', (s) => (chat = s));
  await channel.refresh('request');
  const sending = channel.respond('request', draft());
  assert.equal(mobile.busy, true);
  assert.equal(chat.busy, true);
  await channel.respond('request', draft());
  assert.equal(sends, 1);
  current.response = { response: buildHITLResponse(payload, draft()) };
  current.answerable = false;
  current.transport_state = 'accepted';
  finish(current);
  await sending;
  assert.equal(chat.view?.transport_state, 'accepted');
  await channel.respond('request', draft());
  assert.equal(sends, 1);
  let next: HITLState | undefined;
  channel.subscribe('next', (s) => (next = s));
  assert.equal(next?.view, null);
});

test('network uncertainty and stale reads block resubmission; receipts resolve uncertainty', async () => {
  const current = view();
  let sends = 0,
    failRead = false;
  const channel = createHITLChannel(
    async () => {
      if (failRead) throw new Error('offline');
      return current;
    },
    async () => {
      sends++;
      throw new Error('network');
    },
    () => 'action'
  );
  let latest = state(current);
  channel.subscribe('request', (s) => (latest = s));
  await channel.refresh('request');
  await channel.respond('request', draft());
  assert.equal(latest.uncertain, true);
  await channel.respond('request', draft());
  assert.equal(sends, 1);
  current.response = { response: buildHITLResponse(payload, draft()) };
  current.answerable = false;
  await channel.refresh('request');
  assert.equal(latest.uncertain, false);
  failRead = true;
  await channel.refresh('request');
  assert.equal(latest.stale, true);
  await channel.respond('request', draft());
  assert.equal(sends, 1);
});

test('another-device conflict refreshes the immutable decision', async () => {
  const current = view();
  const channel = createHITLChannel(
    async () => current,
    async () => {
      current.response = { response: buildHITLResponse(payload, draft()) };
      current.answerable = false;
      current.transport_state = 'accepted';
      throw Object.assign(new Error('Already answered'), { status: 409 });
    },
    () => 'action'
  );
  let latest = state(current);
  channel.subscribe('request', (s) => (latest = s));
  await channel.refresh('request');
  await channel.respond('request', draft());
  assert.equal(latest.view?.answerable, false);
  assert.equal(latest.uncertain, false);
});

async function renderer() {
  const source = await readFile(
    new URL('./components/HITLRequest.svelte', import.meta.url),
    'utf8'
  );
  const result = compile(source, { filename: 'HITLRequest.svelte', generate: 'server' });
  // Keep the temporary module under node_modules so Svelte's runtime resolves normally.
  const dir = await mkdtemp(resolve('node_modules/.hitl-render-'));
  const file = resolve(dir, 'request.mjs');
  await writeFile(
    file,
    result.js.code.replace("'../hitl'", JSON.stringify(new URL('./hitl.ts', import.meta.url).href))
  );
  const component = (await import(pathToFileURL(file).href)).default;
  return {
    html: (s: HITLState) =>
      render(component, { props: { snapshot: s, onRespond: () => {}, onRefresh: () => {} } }).body,
    close: () => rm(dir, { recursive: true, force: true })
  };
}

test('shared renderer: read-only states, escaped metadata, provider reasons and merge slots', async () => {
  const r = await renderer();
  try {
    const v = view();
    v.context = { provider: 'codex' };
    let html = r.html(state(v));
    assert.match(html, /runtime receives rejection only/);
    assert.match(html, /&lt;script>/);
    assert.doesNotMatch(html, /<script>/);
    v.context.provider = 'claude';
    v.response = { response: buildHITLResponse(payload, draft()) };
    v.transport_state = 'uncertain';
    html = r.html(state(v));
    assert.match(html, /Claude’s denial message/);
    assert.match(html, /Keep tests/);
    assert.match(html, /Decision delivery uncertain/);
    assert.doesNotMatch(html, />Send response</);
    v.response = null;
    v.writes_enabled = false;
    html = r.html(state(v));
    assert.match(html, /Responding disabled/);
    assert.match(html, /<fieldset disabled/);
    v.request.payload = { type: 'bad' };
    assert.match(r.html(state(v)), /Malformed or unsupported/);
    v.merge = {
      pr_url: 'javascript:alert(1)',
      head: 'abc',
      base: 'def',
      protected_matches: ['k8s/a'],
      ci_evidence: 'pending'
    };
    html = r.html(state(v));
    assert.match(html, /Merge context/);
    assert.doesNotMatch(html, /href="javascript/);
    assert.doesNotMatch(html, />Merge</);
    assert.match(reasonNotice(null), /not verified/);
    assert.match(
      hitlStatus({ ...view(), request: { ...view().request, availability: 'stale' } }),
      /stale/
    );
  } finally {
    await r.close();
  }
});

test(
  'renders observer/API-created requests from the isolated PostgreSQL integration',
  { skip: !process.env.HITL_UI_FIXTURE_PATH },
  async () => {
    const fixtures: Record<string, HITLView> = JSON.parse(
      await readFile(process.env.HITL_UI_FIXTURE_PATH!, 'utf8')
    );
    const r = await renderer();
    try {
      assert.match(r.html(state(fixtures.pending)), /ordinary.tool/);
      assert.match(r.html(state(fixtures.disabled)), /Responding disabled/);
      assert.match(r.html(state(fixtures.stale)), /Observation stale/);
      assert.match(r.html(state(fixtures.unavailable)), /Request unavailable/);
      assert.match(r.html(state(fixtures.answered)), /Decision delivered/);
      const html = r.html(state(fixtures.questions));
      assert.match(html, /Linux/);
      assert.match(html, /macOS/);
      assert.match(html, /Your answer/);
      assert.match(html, /future_metadata/);
      assert.doesNotMatch(html, /<script>/);
      assert.match(r.html(state(fixtures.question_answered)), /Keep tests/);
      const result = buildHITLResponse(fixtures.questions.request.payload, {
        decisions: {},
        reasons: {},
        answers: [['Linux', 'macOS'], ['Keep tests']]
      });
      assert.deepEqual(result, fixtures.question_answered.response?.response);
    } finally {
      await r.close();
    }
  }
);

test('opaque tool IDs do not inherit object prototype values', () => {
  const p = { type: 'tool_approval_request', tools: [{ ...payload.tools[0], id: '__proto__' }] };
  const decisions = Object.assign(Object.create(null), { ['__proto__']: 'reject' });
  assert.deepEqual(buildHITLResponse(p, { decisions, reasons: {}, answers: [] }), {
    type: 'tool_approval_response',
    approvals: [{ id: '__proto__', approved: false }]
  });
  assert.equal(parseHITL({ ...payload, hint: { not: 'text' } }), null);
});

test('an older in-flight read cannot undo a recorded response', async () => {
  const pending = view();
  let reads = 0,
    finishRead!: (v: HITLView) => void;
  const accepted: HITLView = {
    ...pending,
    answerable: false,
    transport_state: 'accepted',
    response: { response: buildHITLResponse(payload, draft()) }
  };
  const channel = createHITLChannel(
    async () => {
      reads++;
      return reads === 2 ? new Promise((resolve) => (finishRead = resolve)) : pending;
    },
    async () => accepted,
    () => 'action'
  );
  let latest = state(pending);
  channel.subscribe('request', (s) => (latest = s));
  await channel.refresh('request');
  const poll = channel.refresh('request');
  await channel.respond('request', draft());
  finishRead(pending);
  await poll;
  assert.equal(latest.view?.transport_state, 'accepted');
  assert.equal(latest.view?.answerable, false);
});

test('automatic reads stop for delivered receipts but continue for uncertain decisions', async () => {
  const current = view();
  let reads = 0;
  current.response = { response: buildHITLResponse(payload, draft()) };
  current.answerable = false;
  current.transport_state = 'uncertain';
  const channel = createHITLChannel(
    async () => {
      reads++;
      return structuredClone(current);
    },
    async () => current
  );
  const signal = new AbortController().signal;
  assert.equal(await channel.poll('request', signal), true);
  assert.equal(await channel.poll('request', signal), true);
  assert.equal(reads, 2);
  current.transport_state = 'accepted';
  assert.equal(await channel.poll('request', signal), false);
  assert.equal(await channel.poll('request', signal), false);
  assert.equal(reads, 3);
});

test('intentional read cancellation preserves state; a read timeout disables stale controls', async () => {
  const current = view();
  let fail = false;
  const channel = createHITLChannel(
    async () => {
      if (fail) throw new Error('aborted');
      return current;
    },
    async () => current
  );
  let latest = state(current);
  channel.subscribe('request', (s) => (latest = s));
  await channel.refresh('request');
  fail = true;
  const hidden = new AbortController();
  hidden.abort();
  await channel.refresh('request', hidden.signal);
  assert.equal(latest.stale, false);
  const deadline = new AbortController();
  deadline.abort(new DOMException('Timed out', 'TimeoutError'));
  await channel.refresh('request', deadline.signal);
  assert.equal(latest.stale, true);
});
