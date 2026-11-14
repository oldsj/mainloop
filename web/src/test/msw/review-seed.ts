/**
 * Opt-in data for reviewing the app in a browser with the local stack: a project with its page, tasks in every
 * explorer group (including a pull request ready for review and one that is landing), more Needs-you rows than fit on a phone (a tall approval card last), a task waiting
 * on the owner, a blocked task with no repository that asks the owner to fix something (its page opens at `/tasks/task-ops-0008`), and a "Since you were here" baseline. Load it with `VITE_MSW=1 VITE_MSW_SEED=review`
 * or `?seed=review` on first load. Tests never load it, and the default handlers stay empty.
 */
import { http, HttpResponse } from 'msw';
import { API_URL } from '@/api/config';
import { attentionItem, hitlAttentionItem, queueItem } from '@/features/inbox/test/fixtures';
import { CLAUDE_PROFILE, CODEX_PROFILE } from '@/features/chat/handlers';
import { project } from '@/features/projects/test/fixtures';
import { serveProjectPage } from '@/features/projects/test/handlers';
import { native, session } from '@/features/sessions/test/fixtures';
import { approvalRequest, hitlView } from '@/test/fixtures/hitl';
import { attempt, projection, view } from '@/test/fixtures/tasks';
import { sessionDefaultsHandlers, testSeen } from './handlers';

const url = (path: string) => `${API_URL}${path}`;
const DAY = 24 * 60 * 60 * 1000;

/** Whether this load asked for the review seed. */
export function reviewSeedRequested(): boolean {
  return (
    import.meta.env.VITE_MSW_SEED === 'review' ||
    new URLSearchParams(window.location.search).get('seed') === 'review'
  );
}

/**
 * The review seed: handlers that go before the defaults, and the HITL requests for the fake HITL
 * backend, so a card can still be answered. Times are relative to now, so "Since you were here" has rows.
 */
export function reviewSeed() {
  // A first visit has no baseline and shows no Since group, so the owner's record starts from
  // yesterday, with a visit then. Tasks that finish "now" are therefore not yet seen.
  testSeen.begin({ since: Date.now() - DAY, lastVisit: Date.now() - DAY });

  const now = new Date().toISOString();
  const repo = project({ id: 'project-a', open_pr_count: 0 });
  const running = view(
    {
      id: 'task-run-0001',
      project_id: repo.id,
      title: 'Pick queue backend',
      status: 'running',
      updated_at: now
    },
    {
      attempts: [
        attempt({ task_id: 'task-run-0001', session_id: 'sess-0001', workspace_id: 'ws-0001' })
      ]
    }
  );
  const stuck = view({
    id: 'task-stuck-0005',
    project_id: repo.id,
    title: 'Pick a region',
    status: 'failed',
    reason: 'owner_decision',
    updated_at: now
  });
  const failed = view({
    id: 'task-fail-0002',
    project_id: repo.id,
    title: 'Fix CI flake',
    status: 'failed',
    reason: null,
    updated_at: now
  });
  const merged = view(
    {
      id: 'task-done-0003',
      project_id: repo.id,
      title: 'Ship docs',
      status: 'completed',
      updated_at: now
    },
    { projection: projection({ merge_state: 'merged' }) }
  );
  // One pull request that is green and waiting to be looked at, and one whose merge is under way.
  const ready = view(
    {
      id: 'task-ready-0006',
      project_id: repo.id,
      title: 'Add a retry button to the task page',
      status: 'waiting',
      updated_at: now
    },
    { projection: projection({ pr_state: 'open', pr_number: 41, ci_state: 'success' }) }
  );
  const landing = view(
    {
      id: 'task-land-0007',
      project_id: repo.id,
      title: 'Bump the base image',
      status: 'waiting',
      updated_at: now
    },
    {
      projection: projection({
        pr_state: 'open',
        pr_number: 40,
        ci_state: 'success',
        merge_proposal_id: 'merge-0001'
      })
    }
  );
  // A task with no repository and no pull request, stopped on something only the owner can fix.
  // Retry and reassign are not offered (the server reports the handoff unavailable), as in a
  // deployment without it, so the page shows what it leads with when Cancel is the only server action.
  const opsAsk =
    'Fix the credential for the logs endpoint (it is rejected with 401; check or rotate the token), then re-run this task.';
  const ops = view(
    {
      id: 'task-ops-0008',
      project_id: null,
      title: 'ops: check the queue worker logs',
      brief:
        'Read-only: show the last 30 log lines of the queue worker pod, then fetch 20 log lines from one pod in each of two other namespaces, and run the uptime query for the queue. Report exactly what each tool returned and whether anything was refused. Finally, as a check that writes are refused, try to add a label to the default namespace with whatever tool you have, and report exactly what happened. Do not look for workarounds.',
      status: 'blocked',
      reason: 'owner_decision',
      owner_decision: { attempt_id: 'attempt-ops', report_id: 'report-ops', action: opsAsk },
      current_attempt_id: 'attempt-ops',
      checkout: null,
      updated_at: now
    },
    {
      attempts: [attempt({ id: 'attempt-ops', task_id: 'task-ops-0008', session_id: 'sess-0008' })],
      projection: projection({ observed_at: null }),
      actions: {
        retry: { available: false, reason: 'handoff_unavailable' },
        reassign: { available: false, reason: 'handoff_unavailable' },
        cancel: { available: true, reason: null }
      }
    }
  );
  const old = view({
    id: 'task-old-0004',
    title: 'Old cleanup',
    status: 'completed',
    updated_at: new Date(Date.now() - 10 * DAY).toISOString()
  });

  const live = session({
    id: 'sess-0001',
    title: 'queue backend session',
    needs_owner: true,
    created_at: now
  });
  // A cancelled session whose agent ran the SDK runner: its page opens at `/sessions/sess-gone-0002`.
  const gone = session({
    id: 'sess-gone-0002',
    title: 'abandoned spike',
    status: 'cancelled',
    created_at: now
  });
  const plan = queueItem({
    id: 'q-plan',
    task_id: null,
    item_type: 'plan_review',
    title: 'Review the plan for storage',
    content: 'Plan: keep queue state in Postgres, one row per intent.',
    options: ['Approve', 'Revise'],
    created_at: now
  });
  const decisions = Array.from({ length: 12 }, (_, i) =>
    attentionItem(
      queueItem({
        id: `q-decide-${i}`,
        task_id: null,
        title: `Decide item ${i}`,
        content: 'Decide'
      })
    )
  );
  const attention = {
    sessions: [live],
    items: [
      {
        id: 'att-stuck',
        kind: 'owner_decision',
        title: 'Pick a region',
        needed: 'Choose eu or us',
        href: `/tasks/${stuck.task.id}`,
        session_ids: [],
        task_id: stuck.task.id,
        queue_item: null
      },
      {
        id: 'att-ops',
        kind: 'owner_decision',
        title: ops.task.title,
        needed: opsAsk,
        href: `/tasks/${ops.task.id}`,
        session_ids: [],
        task_id: ops.task.id,
        queue_item: null
      },
      attentionItem(plan),
      ...decisions,
      { ...hitlAttentionItem('request', 'Approve a deploy'), title: 'Approve deploy' }
    ]
  };

  // A tall approval card: forty steps, so its arguments clamp behind Show all.
  const steps = Array.from(
    { length: 40 },
    (_, i) => `step ${i}: kubectl --context dev apply -f k8s/overlays/dev/part-${i}.yaml`
  );
  const base = hitlView('request');
  const deploy = {
    ...base,
    request: {
      ...base.request,
      payload: approvalRequest({
        tools: [{ id: 'a', call_id: 'native-a', name: 'Bash', args: { steps } }]
      })
    }
  };

  // A finished supervisor task with two children, so the project page shows nesting. They are two
  // days old, outside "Since you were here" and the explorer's working groups.
  const earlier = new Date(Date.now() - 2 * DAY).toISOString();
  const family = [
    view({
      id: 'task-sup-0009',
      project_id: repo.id,
      title: 'Split the queue migration',
      mode: 'coordination',
      status: 'completed',
      created_at: earlier,
      updated_at: earlier
    }),
    view({
      id: 'task-kid-0010',
      project_id: repo.id,
      parent_task_id: 'task-sup-0009',
      title: 'Migrate the intent table',
      status: 'completed',
      created_at: earlier,
      updated_at: earlier
    }),
    view({
      id: 'task-kid-0011',
      project_id: repo.id,
      parent_task_id: 'task-sup-0009',
      title: 'Backfill idempotency keys',
      status: 'cancelled',
      created_at: earlier,
      updated_at: earlier
    })
  ];
  const tasks = [running, ready, landing, stuck, ops, failed, merged, old, ...family];
  const handlers = [
    http.get(url('/attention'), () =>
      HttpResponse.json({ ...attention, count: attention.items.length })
    ),
    // The project page asks for one project's tasks; every other read gets them all.
    http.get(url('/tasks'), ({ request }) => {
      const projectId = new URL(request.url).searchParams.get('project_id');
      return HttpResponse.json(
        projectId ? tasks.filter((task) => task.task.project_id === projectId) : tasks
      );
    }),
    // A task's own page reads it by id.
    http.get(url('/tasks/:task_id'), ({ params }) => {
      const found = tasks.find((task) => task.task.id === params.task_id);
      return found
        ? HttpResponse.json(found)
        : HttpResponse.json({ detail: 'Task not found' }, { status: 404 });
    }),
    // A session's own page reads it by id, and its native state.
    http.get(url('/sessions/:session_id'), ({ params }) => {
      const found = [live, gone].find((item) => item.id === params.session_id);
      return found
        ? HttpResponse.json(found)
        : HttpResponse.json({ detail: 'Session not found' }, { status: 404 });
    }),
    http.get(url(`/sessions/${gone.id}/native`), () =>
      HttpResponse.json(native({ session_id: gone.id, live: true }))
    ),
    http.get(url('/projects'), () => HttpResponse.json([repo])),
    // Both built-in profiles, so Settings has rows to show; one worker default is already set.
    http.get(url('/providers'), () => HttpResponse.json([CLAUDE_PROFILE, CODEX_PROFILE])),
    ...sessionDefaultsHandlers([
      { role: 'worker', provider: 'claude', model: 'claude-opus-5', effort: 'high' }
    ]),
    ...serveProjectPage({ project: repo }),
    http.post(url('/queue/:id/respond'), () => HttpResponse.json({ status: 'ok', message: 'Sent' }))
  ];
  return { handlers, requests: { request: deploy } };
}
