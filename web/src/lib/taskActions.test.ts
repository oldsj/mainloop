import { expect, test } from 'vitest';
import type { TaskActionBody, TaskOperation } from './contract';
import {
  CLAUDE_PROFILE,
  CODEX_PROFILE,
  attempt,
  codexView,
  operation,
  profile,
  reassignedView,
  view
} from '../test/fixtures/tasks';
import {
  actionAvailability,
  classifyFailure,
  createActionTracker,
  createIntent,
  reassignTargets,
  type ActionIntent
} from './taskActions';

class HttpError extends Error {
  status: number;
  reason: string | null;
  constructor(status: number, reason: string | null = null) {
    super(reason ?? `HTTP ${status}`);
    this.status = status;
    this.reason = reason;
  }
}

test('an intent carries the version and attempt the owner saw', () => {
  const v = view();
  const intent = createIntent('retry', v);
  expect(intent.body.expected_version).toBe(3);
  expect(intent.body.expected_attempt_id).toBe('attempt-1');
  expect(intent.body.request_id).toMatch(/^ui-/);
  expect('target_profile_id' in intent.body).toBe(false);

  const noAttempt = createIntent('retry', view({ current_attempt_id: null }));
  expect(noAttempt.body.expected_attempt_id).toBe(null);
});

test('reassign requires an explicit target and sends it', () => {
  expect(() => createIntent('reassign', view())).toThrow(/Choose the provider/);
  const intent = createIntent('reassign', view(), { targetProfileId: 'codex-default' });
  expect(intent.kind === 'reassign' && intent.body.target_profile_id).toBe('codex-default');
});

test('actions follow server eligibility, and a missing entry is never assumed available', () => {
  const v = view({}, { actions: { retry: { available: false, reason: 'handoff' } } });
  expect(actionAvailability(v, 'retry')).toStrictEqual({
    enabled: false,
    reason: 'provider handoff in progress'
  });
  expect(actionAvailability(v, 'cancel').enabled).toBe(false);
  expect(actionAvailability(view(), 'reassign').enabled).toBe(true);
  expect(actionAvailability(view(), 'reassign', { busy: true }).enabled).toBe(false);
});

test('reassign offers only the other provider, for the role, honouring a constraint', () => {
  const profiles = [CLAUDE_PROFILE, CODEX_PROFILE];
  const claude = view();
  expect(reassignTargets(profiles, claude, claude.attempts[0]!).map((p) => p.id)).toStrictEqual([
    'codex-default'
  ]);
  const codex = codexView();
  expect(reassignTargets(profiles, codex, codex.attempts[0]!).map((p) => p.id)).toStrictEqual([
    'claude-default'
  ]);
  const disabled = [CLAUDE_PROFILE, profile({ enabled: false })];
  expect(reassignTargets(disabled, claude, claude.attempts[0]!)).toStrictEqual([]);
  const noRole = profile({ agents: { supervisor: { namespace: 'a', name: 'b' } } });
  expect(reassignTargets([CLAUDE_PROFILE, noRole], claude, claude.attempts[0]!)).toStrictEqual([]);
  const constrained = view({ provider_constraint: 'claude-default' }, { attempts: [attempt()] });
  expect(reassignTargets(profiles, constrained, constrained.attempts[0]!)).toStrictEqual([]);
  // The superseded predecessor never decides the target: only the current attempt does.
  const re = reassignedView();
  expect(reassignTargets(profiles, re, re.attempts[1]!).map((p) => p.id)).toStrictEqual([
    'claude-default'
  ]);
});

const accepted = (intent: ActionIntent): TaskOperation =>
  operation({ kind: intent.kind as TaskOperation['kind'], request_id: intent.body.request_id });

test('a lost response is resent with the same request ID and the same expectations', async () => {
  const sent: TaskActionBody[] = [];
  let calls = 0;
  const tracker = createActionTracker(async (intent) => {
    sent.push(intent.body);
    calls += 1;
    if (calls === 1) throw new TypeError('network down');
    return accepted(intent);
  });
  const v = view();
  const first = await tracker.submit('reassign', v, { targetProfileId: 'codex-default' });
  expect(first.status).toBe('uncertain');
  expect(tracker.pending('task-1', 'reassign')).toBeTruthy();

  // The task moved on locally, and the owner picked another target: the resend must not change.
  const moved = view({ version: 4 });
  const second = await tracker.submit('reassign', moved, { targetProfileId: 'other' });
  expect(second.status).toBe('accepted');
  expect(sent.length).toBe(2);
  expect(sent[1]).toStrictEqual(sent[0]);
  expect(tracker.pending('task-1', 'reassign')).toBe(null);

  // The next request is new.
  await tracker.submit('reassign', moved, { targetProfileId: 'codex-default' });
  expect(sent[2]!.request_id).not.toBe(sent[0]!.request_id);
  expect(sent[2]!.expected_version).toBe(4);
});

test('a server error is also uncertain and reuses the request ID', async () => {
  const ids: string[] = [];
  let calls = 0;
  const tracker = createActionTracker(async (intent) => {
    ids.push(intent.body.request_id);
    if (++calls === 1) throw new HttpError(503);
    return accepted(intent);
  });
  expect((await tracker.submit('retry', view())).status).toBe('uncertain');
  expect((await tracker.submit('retry', view())).status).toBe('accepted');
  expect(ids[0]).toBe(ids[1]);
});

test('a 409 stale version is a refresh, not a blind retry', async () => {
  const tracker = createActionTracker(async () => {
    throw new HttpError(409, 'stale_task_version');
  });
  const outcome = await tracker.submit('retry', view());
  expect(outcome.status).toBe('failed');
  if (outcome.status !== 'failed') return;
  expect(outcome.failure.kind).toBe('stale');
  expect(outcome.failure.refresh).toBe(true);
  expect(outcome.failure.resendSameRequest).toBe(false);
  expect(outcome.failure.message).toMatch(/changed since you loaded/);
  expect(tracker.pending('task-1', 'retry')).toBe(null);
});

test('after a stale 409 the next submit builds a fresh request from the refreshed task', async () => {
  const sent: TaskActionBody[] = [];
  let calls = 0;
  const tracker = createActionTracker(async (intent) => {
    sent.push(intent.body);
    if (++calls === 1) throw new HttpError(409, 'stale_task_attempt');
    return accepted(intent);
  });
  await tracker.submit('retry', view());
  await tracker.submit('retry', view({ version: 5, current_attempt_id: 'attempt-2' }));
  expect(sent[1]!.request_id).not.toBe(sent[0]!.request_id);
  expect(sent[1]!.expected_version).toBe(5);
  expect(sent[1]!.expected_attempt_id).toBe('attempt-2');
});

test('failures are sorted by what the owner should do', () => {
  expect(classifyFailure(new HttpError(409, 'request_payload_conflict')).kind).toBe('conflict');
  expect(classifyFailure(new HttpError(409, 'branch_writer_exists')).kind).toBe('conflict');
  expect(classifyFailure(new HttpError(404, 'task_not_found')).kind).toBe('not_found');
  expect(classifyFailure(new HttpError(403)).kind).toBe('denied');
  expect(classifyFailure(new HttpError(422, 'provider_unavailable')).kind).toBe('invalid');
  expect(classifyFailure(new HttpError(409, 'provider_unqualified:handoff')).message).toMatch(
    /not qualified yet \(handoff is not proved\)/
  );
  expect(classifyFailure(new TypeError('Failed to fetch')).kind).toBe('uncertain');
  expect(classifyFailure(new HttpError(500)).resendSameRequest).toBe(true);
});

test('a second click while a request is in flight shares it', async () => {
  let calls = 0;
  let release!: () => void;
  const gate = new Promise<void>((resolve) => (release = resolve));
  const tracker = createActionTracker(async (intent) => {
    calls += 1;
    await gate;
    return accepted(intent);
  });
  const a = tracker.submit('cancel', view());
  const b = tracker.submit('cancel', view());
  expect(tracker.busy('task-1', 'cancel')).toBe(true);
  release();
  const [ra, rb] = await Promise.all([a, b]);
  expect(calls).toBe(1);
  expect(ra).toBe(rb);
  expect(tracker.busy('task-1', 'cancel')).toBe(false);
});

test('dismissing forgets an unconfirmed request', async () => {
  const tracker = createActionTracker(async () => {
    throw new TypeError('offline');
  });
  await tracker.submit('retry', view());
  tracker.dismiss('task-1', 'retry');
  expect(tracker.pending('task-1', 'retry')).toBe(null);
});
