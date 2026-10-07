import assert from 'node:assert/strict';
import test from 'node:test';
import type { TaskActionBody, TaskOperation } from './api';
import {
  CLAUDE_PROFILE,
  CODEX_PROFILE,
  attempt,
  codexView,
  operation,
  profile,
  reassignedView,
  view
} from './taskFixtures.ts';
import {
  actionAvailability,
  classifyFailure,
  createActionTracker,
  createIntent,
  reassignTargets,
  type ActionIntent
} from './taskActions.ts';

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
  assert.equal(intent.body.expected_version, 3);
  assert.equal(intent.body.expected_attempt_id, 'attempt-1');
  assert.match(intent.body.request_id, /^ui-/);
  assert.equal('target_profile_id' in intent.body, false);

  const noAttempt = createIntent('retry', view({ current_attempt_id: null }));
  assert.equal(noAttempt.body.expected_attempt_id, null);
});

test('reassign requires an explicit target and sends it', () => {
  assert.throws(() => createIntent('reassign', view()), /Choose the provider/);
  const intent = createIntent('reassign', view(), { targetProfileId: 'codex-default' });
  assert.equal(intent.body.target_profile_id, 'codex-default');
});

test('actions follow server eligibility, and a missing entry is never assumed available', () => {
  const v = view({}, { actions: { retry: { available: false, reason: 'handoff' } } });
  assert.deepEqual(actionAvailability(v, 'retry'), {
    enabled: false,
    reason: 'provider handoff in progress'
  });
  assert.equal(actionAvailability(v, 'cancel').enabled, false);
  assert.equal(actionAvailability(view(), 'reassign').enabled, true);
  assert.equal(actionAvailability(view(), 'reassign', { busy: true }).enabled, false);
});

test('reassign offers only the other provider, for the role, honouring a constraint', () => {
  const profiles = [CLAUDE_PROFILE, CODEX_PROFILE];
  const claude = view();
  assert.deepEqual(
    reassignTargets(profiles, claude, claude.attempts[0]).map((p) => p.id),
    ['codex-default']
  );
  const codex = codexView();
  assert.deepEqual(
    reassignTargets(profiles, codex, codex.attempts[0]).map((p) => p.id),
    ['claude-default']
  );
  const disabled = [CLAUDE_PROFILE, profile({ enabled: false })];
  assert.deepEqual(reassignTargets(disabled, claude, claude.attempts[0]), []);
  const noRole = profile({ agents: { supervisor: { namespace: 'a', name: 'b' } } });
  assert.deepEqual(reassignTargets([CLAUDE_PROFILE, noRole], claude, claude.attempts[0]), []);
  const constrained = view({ provider_constraint: 'claude-default' }, { attempts: [attempt()] });
  assert.deepEqual(reassignTargets(profiles, constrained, constrained.attempts[0]), []);
  // The superseded predecessor never decides the target: only the current attempt does.
  const re = reassignedView();
  assert.deepEqual(
    reassignTargets(profiles, re, re.attempts[1]).map((p) => p.id),
    ['claude-default']
  );
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
  assert.equal(first.status, 'uncertain');
  assert.ok(tracker.pending('task-1', 'reassign'));

  // The task moved on locally, and the owner picked another target: the resend must not change.
  const moved = view({ version: 4 });
  const second = await tracker.submit('reassign', moved, { targetProfileId: 'other' });
  assert.equal(second.status, 'accepted');
  assert.equal(sent.length, 2);
  assert.deepEqual(sent[1], sent[0]);
  assert.equal(tracker.pending('task-1', 'reassign'), null);

  // The next request is new.
  await tracker.submit('reassign', moved, { targetProfileId: 'codex-default' });
  assert.notEqual(sent[2].request_id, sent[0].request_id);
  assert.equal(sent[2].expected_version, 4);
});

test('a server error is also uncertain and reuses the request ID', async () => {
  const ids: string[] = [];
  let calls = 0;
  const tracker = createActionTracker(async (intent) => {
    ids.push(intent.body.request_id);
    if (++calls === 1) throw new HttpError(503);
    return accepted(intent);
  });
  assert.equal((await tracker.submit('retry', view())).status, 'uncertain');
  assert.equal((await tracker.submit('retry', view())).status, 'accepted');
  assert.equal(ids[0], ids[1]);
});

test('a 409 stale version is a refresh, not a blind retry', async () => {
  const tracker = createActionTracker(async () => {
    throw new HttpError(409, 'stale_task_version');
  });
  const outcome = await tracker.submit('retry', view());
  assert.equal(outcome.status, 'failed');
  if (outcome.status !== 'failed') return;
  assert.equal(outcome.failure.kind, 'stale');
  assert.equal(outcome.failure.refresh, true);
  assert.equal(outcome.failure.resendSameRequest, false);
  assert.match(outcome.failure.message, /changed since you loaded/);
  assert.equal(tracker.pending('task-1', 'retry'), null);
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
  assert.notEqual(sent[1].request_id, sent[0].request_id);
  assert.equal(sent[1].expected_version, 5);
  assert.equal(sent[1].expected_attempt_id, 'attempt-2');
});

test('failures are sorted by what the owner should do', () => {
  assert.equal(classifyFailure(new HttpError(409, 'request_payload_conflict')).kind, 'conflict');
  assert.equal(classifyFailure(new HttpError(409, 'branch_writer_exists')).kind, 'conflict');
  assert.equal(classifyFailure(new HttpError(404, 'task_not_found')).kind, 'not_found');
  assert.equal(classifyFailure(new HttpError(403)).kind, 'denied');
  assert.equal(classifyFailure(new HttpError(422, 'provider_unavailable')).kind, 'invalid');
  assert.match(
    classifyFailure(new HttpError(409, 'provider_unqualified:handoff')).message,
    /not qualified yet \(handoff is not proved\)/
  );
  assert.equal(classifyFailure(new TypeError('Failed to fetch')).kind, 'uncertain');
  assert.equal(classifyFailure(new HttpError(500)).resendSameRequest, true);
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
  assert.equal(tracker.busy('task-1', 'cancel'), true);
  release();
  const [ra, rb] = await Promise.all([a, b]);
  assert.equal(calls, 1);
  assert.equal(ra, rb);
  assert.equal(tracker.busy('task-1', 'cancel'), false);
});

test('dismissing forgets an unconfirmed request', async () => {
  const tracker = createActionTracker(async () => {
    throw new TypeError('offline');
  });
  await tracker.submit('retry', view());
  tracker.dismiss('task-1', 'retry');
  assert.equal(tracker.pending('task-1', 'retry'), null);
});
