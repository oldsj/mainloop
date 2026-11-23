/**
 * Opt-in data for reviewing the app in a browser with the local stack: a project with its page, tasks in every
 * explorer group (including a pull request ready for review and one that is landing), more Needs-you rows than fit on a phone (a tall approval card last), a task waiting
 * on the owner, a blocked task with no repository that asks the owner to fix something (its page opens at `/tasks/task-ops-0008`), and a "Since you were here" baseline. Load it with `VITE_MSW=1 VITE_MSW_SEED=review`
 * or `?seed=review` on first load. Add `&slow=1` to hold the task, attention and project reads open, so the loading placeholders stay on screen. Tests never load it, and the default handlers stay empty.
 */
import { delay, http, HttpResponse } from 'msw';
import { API_URL } from '@/api/config';
import { attentionItem, hitlAttentionItem, queueItem } from '@/features/inbox/test/fixtures';
import { CLAUDE_PROFILE, CODEX_PROFILE } from '@/features/chat/handlers';
import { project, supervisorSettings, workerLimit } from '@/features/projects/test/fixtures';
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
  // A second project with a standing supervisor at rest and one worker it directs, so the space's
  // first tab is its supervisor. The first project has none, and keeps its overview tab.
  const infra = project({
    id: 'project-b',
    owner: 'acme',
    name: 'infra',
    full_name: 'acme/infra',
    open_pr_count: 0
  });
  const standing = view(
    {
      id: 'task-standing-0020',
      project_id: infra.id,
      title: 'Supervisor of acme/infra',
      mode: 'coordination',
      standing: true,
      status: 'waiting',
      reason: 'idle',
      created_at: earlier,
      updated_at: earlier
    },
    {
      attempts: [attempt({ task_id: 'task-standing-0020', session_id: 'sess-standing-0020' })],
      actions: {
        retry: { available: true, reason: null },
        reassign: { available: false, reason: 'standing_supervisor' },
        cancel: { available: false, reason: 'standing_supervisor' }
      }
    }
  );
  const directed = view(
    {
      id: 'task-directed-0021',
      project_id: infra.id,
      parent_task_id: standing.task.id,
      title: 'Rotate the registry token',
      status: 'running',
      updated_at: now
    },
    {
      attempts: [attempt({ task_id: 'task-directed-0021', session_id: 'sess-directed-0021' })]
    }
  );
  const standingSessions = [
    session({
      id: 'sess-standing-0020',
      title: 'Supervisor of acme/infra',
      status: 'idle',
      project_id: infra.id,
      created_at: earlier
    }),
    session({
      id: 'sess-directed-0021',
      title: 'Rotate the registry token',
      project_id: infra.id,
      created_at: now
    })
  ];
  const tasks = [
    running,
    ready,
    landing,
    stuck,
    ops,
    failed,
    merged,
    old,
    ...family,
    standing,
    directed
  ];
  // `&slow=1`: these reads never answer, so every loading state can be looked at.
  const slow = new URLSearchParams(window.location.search).get('slow') === '1';
  const never = async () => {
    await delay('infinite');
    return HttpResponse.json(null);
  };
  const held = slow
    ? ['/tasks', '/tasks/:task_id', '/attention', '/projects/:project_id'].map((path) =>
        http.get(url(path), never)
      )
    : [];
  const handlers = [
    ...held,
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
      const found = [live, gone, ...standingSessions].find((item) => item.id === params.session_id);
      if (found) return HttpResponse.json(found);
      // An agent that is still going opens on its session, so every task's session has a page.
      const owner = tasks.find((task) =>
        task.attempts.some((candidate) => candidate.session_id === params.session_id)
      );
      return owner
        ? HttpResponse.json(
            session({
              id: String(params.session_id),
              title: owner.task.title,
              project_id: owner.task.project_id,
              created_at: owner.task.created_at
            })
          )
        : HttpResponse.json({ detail: 'Session not found' }, { status: 404 });
    }),
    http.get(url(`/sessions/${gone.id}/native`), () =>
      HttpResponse.json(native({ session_id: gone.id, live: true }))
    ),
    http.get(url('/projects'), () => HttpResponse.json([repo, infra])),
    // Every project's supervisor is on the default provider until the page saves another choice.
    http.get(url('/projects/:project_id/supervisor/settings'), ({ params }) =>
      HttpResponse.json(
        supervisorSettings({
          project_id: String(params.project_id),
          effective_provider_profile_id: CLAUDE_PROFILE.id,
          allowed_models: CLAUDE_PROFILE.models.map((model) => model.id),
          allowed_effort_levels: [...CLAUDE_PROFILE.effort_levels]
        })
      )
    ),
    http.put(url('/projects/:project_id/supervisor/settings'), async ({ params, request }) => {
      const choice = (await request.json()) as {
        provider_profile_id: string | null;
        model: string | null;
        effort: 'low' | 'medium' | 'high' | null;
      };
      const codex = choice.provider_profile_id === CODEX_PROFILE.id;
      const on = codex ? CODEX_PROFILE : CLAUDE_PROFILE;
      return HttpResponse.json(
        supervisorSettings({
          project_id: String(params.project_id),
          ...choice,
          effective_provider_profile_id: on.id,
          allowed_models: on.models.map((model) => model.id),
          allowed_effort_levels: [...on.effort_levels],
          // A change of provider starts a handoff.
          operation_id: codex ? 'operation-seed-1' : null
        })
      );
    }),
    // The first project is on the default limit; the second set its own.
    http.get(url('/projects/:project_id/worker-limit'), ({ params }) =>
      HttpResponse.json(
        params.project_id === infra.id
          ? workerLimit({
              project_id: infra.id,
              max_workers: 3,
              effective_max_workers: 3,
              workers_running: 1
            })
          : workerLimit({ project_id: String(params.project_id) })
      )
    ),
    http.put(url('/projects/:project_id/worker-limit'), async ({ params, request }) => {
      const { max_workers } = (await request.json()) as { max_workers: number | null };
      return HttpResponse.json(
        workerLimit({
          project_id: String(params.project_id),
          max_workers,
          effective_max_workers: Math.min(max_workers ?? 6, 20)
        })
      );
    }),
    // Both built-in profiles, so Settings has rows to show; one worker default is already set.
    http.get(url('/providers'), () =>
      HttpResponse.json(
        // Both can run a supervisor here, so the project page's provider picker has a choice.
        [CLAUDE_PROFILE, CODEX_PROFILE].map((each) => ({
          ...each,
          agents: {
            child: { namespace: 'agents', name: `${each.id}-child` },
            supervisor: { namespace: 'agents', name: `${each.id}-supervisor` }
          }
        }))
      )
    ),
    ...sessionDefaultsHandlers([
      { role: 'worker', provider: 'claude', model: 'claude-opus-5', effort: 'high' }
    ]),
    ...serveProjectPage({ project: repo }),
    http.post(url('/queue/:id/respond'), () => HttpResponse.json({ status: 'ok', message: 'Sent' }))
  ];
  return { handlers, requests: { request: deploy } };
}
