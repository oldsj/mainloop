import assert from 'node:assert/strict';
import test from 'node:test';
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
} from './taskFixtures.ts';
import {
  STALE_OBSERVATION_MS,
  UNVERIFIED_SUMMARY_LABEL,
  ancestors,
  attemptHistory,
  buildTree,
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
} from './taskState.ts';

void CLAUDE_PROFILE;

test('both providers show their own native identity and workspace', () => {
  const claude = view();
  const codex = codexView();
  assert.equal(providerLabel(claude), 'claude');
  assert.equal(providerLabel(codex), 'codex');
  assert.equal(workspaceFacts(claude).sessionId, 'session-1');
  assert.equal(workspaceFacts(codex).sessionId, 'session-codex');
  assert.equal(workspaceFacts(codex).workspaceId, 'workspace-codex');
  assert.equal(workspaceFacts(claude).branch, 'task/retry');
});

test('a task with no attempt falls back to its assigned profile', () => {
  const queued = view({ status: 'queued', current_attempt_id: null }, { attempts: [] });
  assert.equal(providerLabel(queued), 'claude-default');
  assert.equal(workspaceFacts(queued).workspaceId, null);
});

test('observed branch wins over the requested checkout branch', () => {
  const observed = view(
    {},
    { projection: projection({ branch: 'actual', repository: 'org/repo' }) }
  );
  assert.equal(workspaceFacts(observed).branch, 'actual');
  assert.equal(workspaceFacts(observed).repository, 'org/repo');
});

test('superseded attempts stay in the history, newest first, with their lineage', () => {
  const rows = attemptHistory(reassignedView());
  assert.deepEqual(
    rows.map((row) => [row.attempt.number, row.current, row.superseded, row.predecessorNumber]),
    [
      [2, true, false, 1],
      [1, false, true, null]
    ]
  );
  assert.equal(rows[1].attempt.checkpoint_ref, 'refs/mainloop/checkpoints/c1');
});

test('a session of a superseded attempt still resolves to its task, flagged not current', () => {
  const views = [reassignedView(), view()];
  const old = taskForSession(views, 'session-old');
  assert.equal(old?.view.task.id, 'task-reassigned');
  assert.equal(old?.current, false);
  assert.equal(taskForSession(views, 'session-new')?.current, true);
  assert.equal(taskForWorkspace(views, 'workspace-1')?.view.task.id, 'task-1');
  assert.equal(taskForSession(views, 'missing'), null);
});

test('the unverified provider summary carries its label and is bounded', () => {
  const summary = unverifiedSummary(reassignedView());
  assert.equal(summary?.label, UNVERIFIED_SUMMARY_LABEL);
  assert.equal(summary?.label, 'Provider summary; unverified');
  assert.match(summary?.text ?? '', /tests pass/);

  const long = reassignedView();
  long.artifacts[0].payload = { summary: 'x'.repeat(20000) };
  assert.ok((unverifiedSummary(long)?.text.length ?? 0) <= 8193);
  assert.equal(unverifiedSummary(view()), null);

  const empty = reassignedView();
  empty.artifacts[0].payload = { summary: '   ' };
  assert.equal(unverifiedSummary(empty), null);
});

test('reports are claims; a completed report does not make the task completed', () => {
  assert.match(
    reportLabel({
      task_id: 'task-1',
      attempt_id: 'attempt-1',
      request_id: 'r',
      summary: 'done',
      outcome: 'completed'
    }),
    /unverified claim/
  );
  const claimed = view(
    { status: 'waiting', reason: 'ci' },
    {
      reports: [
        {
          task_id: 'task-1',
          attempt_id: 'attempt-1',
          request_id: 'r',
          summary: 'done',
          outcome: 'completed'
        }
      ]
    }
  );
  assert.equal(statusLabel(claimed.task.status), 'Waiting');
});

test('missing PR, CI and merge fields are unknown, not passing or absent', () => {
  const facts = publicationFacts(undefined, NOW);
  assert.equal(facts.pr.known, false);
  assert.equal(facts.pr.label, 'Unknown');
  assert.equal(facts.ci.known, false);
  assert.equal(facts.merge.known, false);
  assert.equal(facts.merge.label, 'Not reported');
  assert.equal(facts.publication.known, false);
  assert.equal(facts.prUrl, null);
  assert.equal(facts.observation.known, false);
  assert.equal(facts.observation.stale, true);
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
  assert.equal(facts.pr.label, '#12 open');
  assert.equal(facts.ci.label, 'Running');
  assert.equal(facts.ci.known, true);
  assert.notEqual(facts.ci.tone, 'ok');
  assert.equal(facts.merge.label, 'Blocked');

  const merged = publicationFacts(
    projection({ pr_state: 'merged', ci_state: 'failure', pr_head_sha: 'aaa', ci_head_sha: 'aaa' }),
    NOW
  );
  assert.equal(merged.pr.tone, 'ok');
  assert.equal(merged.ci.tone, 'bad');
});

test('explicit unknown states stay unknown', () => {
  const facts = publicationFacts(projection({ pr_state: 'unknown', ci_state: 'unknown' }), NOW);
  assert.equal(facts.pr.known, false);
  assert.equal(facts.ci.known, false);
});

test('CI observed for another commit says nothing about the PR head', () => {
  const facts = publicationFacts(
    projection({ pr_state: 'open', pr_head_sha: 'bbb', ci_state: 'success', ci_head_sha: 'aaa' }),
    NOW
  );
  assert.equal(facts.ciHeadMismatch, true);
  assert.equal(facts.ci.known, false);
  assert.match(facts.ci.label, /different commit/);

  const same = publicationFacts(
    projection({ pr_head_sha: 'aaa', ci_state: 'success', ci_head_sha: 'aaa' }),
    NOW
  );
  assert.equal(same.ci.label, 'Passing');
});

test('observation age is relative and goes stale', () => {
  const fresh = observationAge('2026-01-01T11:59:00Z', NOW);
  assert.equal(fresh.text, 'observed 1m ago');
  assert.equal(fresh.stale, false);
  const old = observationAge(new Date(NOW - STALE_OBSERVATION_MS - 1000).toISOString(), NOW);
  assert.equal(old.stale, true);
  assert.equal(old.known, true);
  assert.equal(observationAge('garbage', NOW).known, false);
  assert.equal(observationAge(null, NOW).text, 'not observed yet');
});

test('only https PR links are rendered as links', () => {
  assert.equal(safeExternalUrl('https://github.com/o/r/pull/1'), 'https://github.com/o/r/pull/1');
  assert.equal(safeExternalUrl('javascript:alert(1)'), null);
  assert.equal(safeExternalUrl('http://github.com/o/r/pull/1'), null);
  assert.equal(safeExternalUrl('not a url'), null);
  assert.equal(publicationFacts(projection({ pr_url: 'javascript:alert(1)' }), NOW).prUrl, null);
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
  assert.deepEqual(
    rows.map((row) => [row.view.task.id, row.depth, row.detached]),
    [
      ['p', 0, false],
      ['a', 1, false],
      ['b', 1, false],
      ['o', 0, true]
    ]
  );
  assert.deepEqual(
    ancestors([parent, childA], 'a').map((v) => v.task.id),
    ['p']
  );
});

test('a parent cycle cannot loop or drop tasks', () => {
  const a = view({ id: 'a', parent_task_id: 'b' });
  const b = view({ id: 'b', parent_task_id: 'a' });
  const rows = buildTree([a, b]);
  assert.equal(rows.length, 2);
});

test('operation progress shows the step and holds blocked and uncertain at the last confirmed step', () => {
  const moving = operationProgress(
    operation({ state: 'target_creating', last_confirmed_step: 'source_fenced' })
  );
  assert.equal(moving.held, false);
  assert.equal(moving.steps.find((s) => s.state === 'target_creating')?.status, 'current');
  assert.equal(moving.steps.find((s) => s.state === 'source_fenced')?.status, 'done');
  assert.equal(moving.steps.find((s) => s.state === 'completed')?.status, 'pending');

  const uncertain = operationProgress(
    operation({
      state: 'uncertain',
      last_confirmed_step: 'source_fencing',
      reason: 'reconciliation'
    })
  );
  assert.equal(uncertain.held, true);
  assert.match(uncertain.summary, /reconciling uncertain state/);
  assert.equal(uncertain.steps.find((s) => s.state === 'target_creating')?.status, 'pending');

  const done = operationProgress(
    operation({ state: 'completed', last_confirmed_step: 'completed' })
  );
  assert.equal(done.open, false);
  assert.ok(done.steps.every((s) => s.status === 'done'));
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
  assert.equal(openOperations(blocked).length, 1);
  const blockers = switchBlockers(blocked);
  assert.ok(blockers.some((b) => /2 pending approvals/.test(b)));
  assert.ok(blockers.some((b) => /Checkpoint required/.test(b)));
  assert.deepEqual(switchBlockers(view()), []);
});

test('inbox cards find the task whose projection lists their approval', () => {
  const waiting = view({}, { projection: projection({ pending_approval_ids: ['hitl-9'] }) });
  assert.equal(taskForApproval([view(), waiting], 'hitl-9'), waiting);
  assert.equal(taskForApproval([view()], 'hitl-9'), null);
});

test('labels degrade for values the UI does not know', () => {
  assert.equal(statusLabel('exploded'), 'Unknown (exploded)');
  assert.equal(reasonLabel('new_reason'), 'new reason');
  assert.equal(reasonLabel(null), null);
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
    assert.equal(ci.known, false);
    assert.notEqual(ci.label, 'Passing');
    assert.equal(ci.known ? ci.label.toLowerCase() : 'unknown', 'unknown');
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
      assert.equal(ci.known, false);
    }
    const ci = publicationFacts(
      projection({
        ci_state,
        pr_head_sha: 'aaa',
        ci_head_sha: 'aaa'
      }),
      NOW
    ).ci;
    assert.equal(ci.known, true);
    assert.equal(ci.label, ci_state === 'failure' ? 'Failing' : 'Running');
  }
});
