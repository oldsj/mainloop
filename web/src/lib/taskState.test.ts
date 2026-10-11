import { expect, test } from 'vitest';
import {
  CLAUDE_PROFILE,
  NOW,
  attempt,
  codexView,
  operation,
  projection,
  reassignedView,
  task,
  view
} from '../test/fixtures/tasks';
import {
  STALE_OBSERVATION_MS,
  UNVERIFIED_SUMMARY_LABEL,
  ancestors,
  attemptHistory,
  buildTree,
  normalizeView,
  observationAge,
  openOperations,
  operationProgress,
  providerLabel,
  publicationFacts,
  reasonLabel,
  reportLabel,
  safeExternalUrl,
  statusLabel,
  switchBlockers,
  taskForApproval,
  taskForSession,
  taskForWorkspace,
  unverifiedSummary,
  workspaceFacts
} from './taskState';

void CLAUDE_PROFILE;

test('both providers show their own native identity and workspace', () => {
  const claude = view();
  const codex = codexView();
  expect(providerLabel(claude)).toBe('claude');
  expect(providerLabel(codex)).toBe('codex');
  expect(workspaceFacts(claude).sessionId).toBe('session-1');
  expect(workspaceFacts(codex).sessionId).toBe('session-codex');
  expect(workspaceFacts(codex).workspaceId).toBe('workspace-codex');
  expect(workspaceFacts(claude).branch).toBe('task/retry');
});

test('a task with no attempt falls back to its assigned profile', () => {
  const queued = view({ status: 'queued', current_attempt_id: null }, { attempts: [] });
  expect(providerLabel(queued)).toBe('claude-default');
  expect(workspaceFacts(queued).workspaceId).toBe(null);
});

test('observed branch wins over the requested checkout branch', () => {
  const observed = view(
    {},
    { projection: projection({ branch: 'actual', repository: 'org/repo' }) }
  );
  expect(workspaceFacts(observed).branch).toBe('actual');
  expect(workspaceFacts(observed).repository).toBe('org/repo');
});

test('superseded attempts stay in the history, newest first, with their lineage', () => {
  const rows = attemptHistory(reassignedView());
  expect(
    rows.map((row) => [row.attempt.number, row.current, row.superseded, row.predecessorNumber])
  ).toStrictEqual([
    [2, true, false, 1],
    [1, false, true, null]
  ]);
  expect(rows[1]!.attempt.checkpoint_ref).toBe('refs/mainloop/checkpoints/c1');
});

test('a session of a superseded attempt still resolves to its task, flagged not current', () => {
  const views = [reassignedView(), view()];
  const old = taskForSession(views, 'session-old');
  expect(old?.view.task.id).toBe('task-reassigned');
  expect(old?.current).toBe(false);
  expect(taskForSession(views, 'session-new')?.current).toBe(true);
  expect(taskForWorkspace(views, 'workspace-1')?.view.task.id).toBe('task-1');
  expect(taskForSession(views, 'missing')).toBe(null);
});

test('the unverified provider summary carries its label and is bounded', () => {
  const summary = unverifiedSummary(reassignedView());
  expect(summary?.label).toBe(UNVERIFIED_SUMMARY_LABEL);
  expect(summary?.label).toBe('Provider summary; unverified');
  expect(summary?.text ?? '').toMatch(/tests pass/);

  const long = reassignedView();
  long.artifacts[0]!.payload = { summary: 'x'.repeat(20000) };
  expect((unverifiedSummary(long)?.text.length ?? 0) <= 8193).toBeTruthy();
  expect(unverifiedSummary(view())).toBe(null);

  const empty = reassignedView();
  empty.artifacts[0]!.payload = { summary: '   ' };
  expect(unverifiedSummary(empty)).toBe(null);
});

test('reports are claims; a completed report does not make the task completed', () => {
  expect(
    reportLabel({
      task_id: 'task-1',
      attempt_id: 'attempt-1',
      request_id: 'r',
      summary: 'done',
      outcome: 'completed',
      owner_action: null,
      evidence_refs: []
    })
  ).toMatch(/unverified claim/);
  const claimed = view(
    { status: 'waiting', reason: 'ci' },
    {
      reports: [
        {
          task_id: 'task-1',
          attempt_id: 'attempt-1',
          request_id: 'r',
          summary: 'done',
          outcome: 'completed',
          owner_action: null,
          evidence_refs: []
        }
      ]
    }
  );
  expect(statusLabel(claimed.task.status)).toBe('Waiting');
});

test('missing PR, CI and merge fields are unknown, not passing or absent', () => {
  const facts = publicationFacts(undefined, NOW);
  expect(facts.pr.known).toBe(false);
  expect(facts.pr.label).toBe('Unknown');
  expect(facts.ci.known).toBe(false);
  expect(facts.merge.known).toBe(false);
  expect(facts.merge.label).toBe('Not reported');
  expect(facts.publication.known).toBe(false);
  expect(facts.prUrl).toBe(null);
  expect(facts.observation.known).toBe(false);
  expect(facts.observation.stale).toBe(true);
});

test('a bare task view reads the same as a payload with no projection', () => {
  // The frozen port kept an empty projection. normalizeView now fills the schema's own unknown
  // defaults (pr_state and ci_state are 'unknown'); no helper may read those as a fact.
  const bare = normalizeView({ task: task() });
  expect(bare.projection.pr_state).toBe('unknown');
  expect(bare.projection.ci_state).toBe('unknown');
  expect(publicationFacts(bare.projection, NOW)).toStrictEqual(publicationFacts({}, NOW));
  expect(workspaceFacts(bare)).toStrictEqual({
    repository: null,
    branch: 'task/retry',
    environmentVersionId: null,
    workspaceId: null,
    sessionId: null
  });
  expect(switchBlockers(bare)).toStrictEqual([]);
  expect(taskForApproval([bare], 'request-1')).toBe(null);
});

test('pending CI is not passing, and PR, CI and merge are independent facts', () => {
  const facts = publicationFacts(
    projection({
      pr_state: 'open',
      pr_number: 12,
      ci_state: 'pending',
      pr_head_sha: 'aaa',
      ci_head_sha: 'aaa',
      merge_state: 'blocked'
    }),
    NOW
  );
  expect(facts.pr.label).toBe('#12 open');
  expect(facts.ci.label).toBe('Running');
  expect(facts.ci.known).toBe(true);
  expect(facts.ci.tone).not.toBe('ok');
  expect(facts.merge.label).toBe('Blocked');

  const merged = publicationFacts(
    projection({ pr_state: 'merged', ci_state: 'failure', pr_head_sha: 'aaa', ci_head_sha: 'aaa' }),
    NOW
  );
  expect(merged.pr.tone).toBe('ok');
  expect(merged.ci.tone).toBe('bad');
});

test('explicit unknown states stay unknown', () => {
  const facts = publicationFacts(projection({ pr_state: 'unknown', ci_state: 'unknown' }), NOW);
  expect(facts.pr.known).toBe(false);
  expect(facts.ci.known).toBe(false);
});

test('CI observed for another commit says nothing about the PR head', () => {
  const facts = publicationFacts(
    projection({ pr_state: 'open', pr_head_sha: 'bbb', ci_state: 'success', ci_head_sha: 'aaa' }),
    NOW
  );
  expect(facts.ciHeadMismatch).toBe(true);
  expect(facts.ci.known).toBe(false);
  expect(facts.ci.label).toMatch(/different commit/);

  const same = publicationFacts(
    projection({ pr_head_sha: 'aaa', ci_state: 'success', ci_head_sha: 'aaa' }),
    NOW
  );
  expect(same.ci.label).toBe('Passing');
});

test('observation age is relative and goes stale', () => {
  const fresh = observationAge('2026-01-01T11:59:00Z', NOW);
  expect(fresh.text).toBe('observed 1m ago');
  expect(fresh.stale).toBe(false);
  const old = observationAge(new Date(NOW - STALE_OBSERVATION_MS - 1000).toISOString(), NOW);
  expect(old.stale).toBe(true);
  expect(old.known).toBe(true);
  expect(observationAge('garbage', NOW).known).toBe(false);
  expect(observationAge(null, NOW).text).toBe('not observed yet');
});

test('only https PR links are rendered as links', () => {
  expect(safeExternalUrl('https://github.com/o/r/pull/1')).toBe('https://github.com/o/r/pull/1');
  expect(safeExternalUrl('javascript:alert(1)')).toBe(null);
  expect(safeExternalUrl('http://github.com/o/r/pull/1')).toBe(null);
  expect(safeExternalUrl('not a url')).toBe(null);
  expect(publicationFacts(projection({ pr_url: 'javascript:alert(1)' }), NOW).prUrl).toBe(null);
});

test('supervisor and child tasks form a tree; a missing parent does not hide the child', () => {
  const parent = view({ id: 'p', root_task_id: 'p', created_at: '2026-01-01T09:00:00Z' });
  const childA = view({
    id: 'a',
    parent_task_id: 'p',
    root_task_id: 'p',
    created_at: '2026-01-01T09:10:00Z'
  });
  const childB = view({
    id: 'b',
    parent_task_id: 'p',
    root_task_id: 'p',
    created_at: '2026-01-01T09:20:00Z'
  });
  const orphan = view({
    id: 'o',
    parent_task_id: 'gone',
    root_task_id: 'gone',
    created_at: '2026-01-01T08:00:00Z'
  });

  const rows = buildTree([childB, orphan, parent, childA]);
  expect(rows.map((row) => [row.view.task.id, row.depth, row.detached])).toStrictEqual([
    ['p', 0, false],
    ['a', 1, false],
    ['b', 1, false],
    ['o', 0, true]
  ]);
  expect(ancestors([parent, childA], 'a').map((v) => v.task.id)).toStrictEqual(['p']);
});

test('a parent cycle cannot loop or drop tasks', () => {
  const a = view({ id: 'a', parent_task_id: 'b' });
  const b = view({ id: 'b', parent_task_id: 'a' });
  const rows = buildTree([a, b]);
  expect(rows.length).toBe(2);
});

test('operation progress shows the step and holds blocked and uncertain at the last confirmed step', () => {
  const moving = operationProgress(
    operation({ state: 'target_creating', last_confirmed_step: 'source_fenced' })
  );
  expect(moving.held).toBe(false);
  expect(moving.steps.find((s) => s.state === 'target_creating')?.status).toBe('current');
  expect(moving.steps.find((s) => s.state === 'source_fenced')?.status).toBe('done');
  expect(moving.steps.find((s) => s.state === 'completed')?.status).toBe('pending');

  const uncertain = operationProgress(
    operation({
      state: 'uncertain',
      last_confirmed_step: 'source_fencing',
      reason: 'reconciliation'
    })
  );
  expect(uncertain.held).toBe(true);
  expect(uncertain.summary).toMatch(/reconciling uncertain state/);
  expect(uncertain.steps.find((s) => s.state === 'target_creating')?.status).toBe('pending');

  const done = operationProgress(
    operation({ state: 'completed', last_confirmed_step: 'completed' })
  );
  expect(done.open).toBe(false);
  expect(done.steps.every((s) => s.status === 'done')).toBeTruthy();
});

test('checkpoint and approval blockers are listed for the owner', () => {
  const blocked = view(
    { status: 'blocked', reason: 'handoff' },
    {
      operations: [
        operation({
          state: 'blocked',
          last_confirmed_step: 'checkpoint_required',
          reason: 'handoff'
        }),
        operation({ id: 'old', state: 'completed', last_confirmed_step: 'completed' })
      ],
      projection: projection({ pending_approval_ids: ['hitl-1', 'hitl-2'] })
    }
  );
  expect(openOperations(blocked).length).toBe(1);
  const blockers = switchBlockers(blocked);
  expect(blockers.some((b) => /2 pending approvals/.test(b))).toBeTruthy();
  expect(blockers.some((b) => /Checkpoint required/.test(b))).toBeTruthy();
  expect(switchBlockers(view())).toStrictEqual([]);
});

test('inbox cards find the task whose projection lists their approval', () => {
  const waiting = view({}, { projection: projection({ pending_approval_ids: ['hitl-9'] }) });
  expect(taskForApproval([view(), waiting], 'hitl-9')).toBe(waiting);
  expect(taskForApproval([view()], 'hitl-9')).toBe(null);
});

test('labels degrade for values the UI does not know', () => {
  expect(statusLabel('exploded')).toBe('Unknown (exploded)');
  expect(reasonLabel('new_reason')).toBe('new reason');
  expect(reasonLabel(null)).toBe(null);
  void task;
  void attempt;
});

test('CI passing requires fresh valid exact-head evidence, including list semantics', () => {
  for (const overrides of [
    { pr_head_sha: null },
    { ci_head_sha: null },
    { observed_at: null },
    { observed_at: 'invalid' },
    { observed_at: '2020-01-01T00:00:00Z' },
    { observed_at: '2099-01-01T00:00:00Z' }
  ]) {
    const ci = publicationFacts(
      projection({ ci_state: 'success', pr_head_sha: 'aaa', ci_head_sha: 'aaa', ...overrides }),
      NOW
    ).ci;
    expect(ci.known).toBe(false);
    expect(ci.label).not.toBe('Passing');
    expect(ci.known ? ci.label.toLowerCase() : 'unknown').toBe('unknown');
  }
});

test('failure and pending CI also require fresh exact-head evidence', () => {
  for (const ci_state of ['failure', 'pending'] as const) {
    for (const evidence of [
      { pr_head_sha: null },
      { ci_head_sha: null },
      { ci_head_sha: 'bbb' },
      { observed_at: null },
      { observed_at: 'invalid' },
      { observed_at: '2020-01-01T00:00:00Z' },
      { observed_at: '2099-01-01T00:00:00Z' }
    ]) {
      const ci = publicationFacts(
        projection({
          ci_state,
          pr_head_sha: 'aaa',
          ci_head_sha: 'aaa',
          ...evidence
        }),
        NOW
      ).ci;
      expect(ci.known).toBe(false);
    }
    const ci = publicationFacts(
      projection({
        ci_state,
        pr_head_sha: 'aaa',
        ci_head_sha: 'aaa'
      }),
      NOW
    ).ci;
    expect(ci.known).toBe(true);
    expect(ci.label).toBe(ci_state === 'failure' ? 'Failing' : 'Running');
  }
});
