/**
 * Opt-in data for reviewing the app in a browser with the local stack: a project with its page, tasks in every
 * explorer group, more Needs-you rows than fit on a phone (a tall approval card last), a task waiting
 * on the owner, and a "Since you were here" baseline. Load it with `VITE_MSW=1 VITE_MSW_SEED=review`
 * or `?seed=review` on first load. Tests never load it, and the default handlers stay empty.
 */
import { http, HttpResponse } from 'msw';
import { API_URL } from '@/api/config';
import { attentionItem, hitlAttentionItem, queueItem } from '@/features/inbox/test/fixtures';
import { project } from '@/features/projects/test/fixtures';
import { serveProjectPage } from '@/features/projects/test/handlers';
import { session } from '@/features/sessions/test/fixtures';
import { approvalRequest, hitlView } from '@/test/fixtures/hitl';
import { attempt, projection, view } from '@/test/fixtures/tasks';

const url = (path: string) => `${API_URL}${path}`;
const DAY = 24 * 60 * 60 * 1000;
const LAST_VISIT_KEY = 'mainloop.home.lastVisit';

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
  // A first visit has no baseline and shows no Since group, so it starts from yesterday.
  try {
    if (!window.localStorage.getItem(LAST_VISIT_KEY)) {
      window.localStorage.setItem(LAST_VISIT_KEY, String(Date.now() - DAY));
    }
  } catch {
    // Storage blocked: the Since group is left out, as it is for the owner.
  }

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

  const handlers = [
    http.get(url('/attention'), () =>
      HttpResponse.json({ ...attention, count: attention.items.length })
    ),
    http.get(url('/tasks'), () => HttpResponse.json([running, stuck, failed, merged, old])),
    http.get(url('/projects'), () => HttpResponse.json([repo])),
    ...serveProjectPage({ project: repo }),
    http.post(url('/queue/:id/respond'), () => HttpResponse.json({ status: 'ok', message: 'Sent' }))
  ];
  return { handlers, requests: { request: deploy } };
}
