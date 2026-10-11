import { expect, test } from 'vitest';
import {
  buildHITLResponse,
  createHITLChannel,
  parseHITL,
  type HITLView,
  type HITLState
} from './hitl';

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
  expect(parseHITL(payload)).toStrictEqual(payload);
  const input = draft();
  expect(buildHITLResponse(payload, input)).toStrictEqual({
    type: 'tool_approval_response',
    approvals: [
      { id: 'a', approved: true },
      { id: 'b', approved: false, rejection_reason: 'Keep tests' }
    ]
  });
  expect(
    buildHITLResponse(payload, {
      ...input,
      reviewedContext: { a: 'f'.repeat(64) }
    })
  ).toStrictEqual({
    type: 'tool_approval_response',
    approvals: [
      { id: 'a', approved: true },
      { id: 'b', approved: false, rejection_reason: 'Keep tests' }
    ],
    reviewed_context: { a: 'f'.repeat(64) }
  });
  expect(() => buildHITLResponse(payload, { ...input, decisions: { a: 'approve' } })).toThrow(
    /every call/
  );
  expect(() =>
    buildHITLResponse(payload, { ...input, decisions: { a: 'approve', z: 'reject' } })
  ).toThrow(/every call/);
  expect(parseHITL({ ...payload, tools: [payload.tools[0], payload.tools[0]] })).toBe(null);
  expect(parseHITL({ type: 'future', tools: payload.tools })).toBe(null);
  expect(parseHITL({ ...payload, tools: [{ id: 'x' }] })).toBe(null);
  expect(parseHITL({ ...payload, nested: {} })).toBe(null);
  const nested = {
    ...payload,
    nested: { task_id: 'child-task', context_id: 'child-context', tools: [payload.tools[1]] }
  };
  expect(
    buildHITLResponse(nested, { decisions: { b: 'reject' }, reasons: {}, answers: [] })
  ).toStrictEqual({ type: 'tool_approval_response', approvals: [{ id: 'b', approved: false }] });
  const approval = buildHITLResponse(payload, { ...input, reasons: { a: 'old rejection', b: '' } });
  expect(
    approval.type === 'tool_approval_response' && approval.approvals[0]!.rejection_reason
  ).toBe(undefined);
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
  expect(buildHITLResponse(question, input)).toStrictEqual({
    type: 'ask_user_response',
    id: 'q',
    answers: [{ answer: ['a', 'b'] }, { answer: ['Because'] }]
  });
  expect(() => buildHITLResponse(question, { ...input, answers: [['a']] })).toThrow(
    /every question/
  );
  expect(() => buildHITLResponse(question, { ...input, answers: [['c'], ['ok']] })).toThrow(
    /offered/
  );
  expect(() => buildHITLResponse(question, { ...input, answers: [['a'], ['one', 'two']] })).toThrow(
    /one answer/
  );
  expect(() => buildHITLResponse(question, { ...input, answers: [['a'], [' ']] })).toThrow(
    /every question/
  );
  const nested = {
    ...question,
    nested: { task_id: 'child', context_id: 'ctx', tools: [payload.tools[0]] }
  };
  expect((buildHITLResponse(nested, input) as { id: string }).id).toBe('a');
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
  expect(mobile.busy).toBe(true);
  expect(chat.busy).toBe(true);
  await channel.respond('request', draft());
  expect(sends).toBe(1);
  current.response = { response: buildHITLResponse(payload, draft()) };
  current.answerable = false;
  current.transport_state = 'accepted';
  finish(current);
  await sending;
  expect(chat.view?.transport_state).toBe('accepted');
  await channel.respond('request', draft());
  expect(sends).toBe(1);
  let next: HITLState | undefined;
  channel.subscribe('next', (s) => (next = s));
  expect(next?.view).toBe(null);
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
  expect(latest.uncertain).toBe(true);
  await channel.respond('request', draft());
  expect(sends).toBe(1);
  current.response = { response: buildHITLResponse(payload, draft()) };
  current.answerable = false;
  await channel.refresh('request');
  expect(latest.uncertain).toBe(false);
  failRead = true;
  await channel.refresh('request');
  expect(latest.stale).toBe(true);
  await channel.respond('request', draft());
  expect(sends).toBe(1);
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
  expect(latest.view?.answerable).toBe(false);
  expect(latest.uncertain).toBe(false);
});

test('4xx refusals show the server detail and end recording even if refresh fails', async () => {
  for (const status of [400, 403, 409, 422]) {
    let failRead = false;
    const channel = createHITLChannel(
      async () => {
        if (failRead) throw new Error('Offline');
        return view();
      },
      async () => {
        failRead = true;
        throw Object.assign(new Error('Merge proposal expired; prepare a fresh operation'), {
          status
        });
      },
      () => 'action'
    );
    let latest = state(view());
    channel.subscribe('request', (s) => (latest = s));
    await channel.refresh('request');
    await channel.respond('request', draft());
    expect(latest.busy).toBe(false);
    expect(latest.uncertain).toBe(false);
    expect(latest.stale).toBe(true);
    expect(latest.error!).toMatch(/Merge proposal expired; prepare a fresh operation/);
  }
});

test('opaque tool IDs do not inherit object prototype values', () => {
  const p = { type: 'tool_approval_request', tools: [{ ...payload.tools[0], id: '__proto__' }] };
  const decisions = Object.assign(Object.create(null), { ['__proto__']: 'reject' });
  expect(buildHITLResponse(p, { decisions, reasons: {}, answers: [] })).toStrictEqual({
    type: 'tool_approval_response',
    approvals: [{ id: '__proto__', approved: false }]
  });
  expect(parseHITL({ ...payload, hint: { not: 'text' } })).toBe(null);
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
  expect(latest.view?.transport_state).toBe('accepted');
  expect(latest.view?.answerable).toBe(false);
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
  expect(await channel.poll('request', signal)).toBe(true);
  expect(await channel.poll('request', signal)).toBe(true);
  expect(reads).toBe(2);
  current.transport_state = 'accepted';
  expect(await channel.poll('request', signal)).toBe(false);
  expect(await channel.poll('request', signal)).toBe(false);
  expect(reads).toBe(3);
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
  expect(latest.stale).toBe(false);
  const deadline = new AbortController();
  deadline.abort(new DOMException('Timed out', 'TimeoutError'));
  await channel.refresh('request', deadline.signal);
  expect(latest.stale).toBe(true);
});
