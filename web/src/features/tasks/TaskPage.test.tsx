import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import type { QueryClient } from '@tanstack/react-query';
import { HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { connection } from '@/api';
import { createSseApplier } from '@/api/invalidation';
import { server } from '@/test/msw/server';
import type { TaskView } from '@/lib/contract';
import { attempt, operation, projection, view } from '@/test/fixtures/tasks';
import { renderTasks } from './test/render';
import {
  problem,
  providersRead,
  serveTaskPage,
  taskAction,
  taskProblem,
  taskRead,
  allowedView,
  taskScreen,
  tasksList
} from './test/handlers';

afterEach(async () => {
  // A lost reply marks the shared connection offline, which would leak into the next test.
  await connection.check();
});

// Reads skip the app's rate limiter here; see fastReads.ts for why this mock sits in each test file.
vi.mock('@/api/middleware', async (importOriginal) => {
  const { withFastReads } = await import('./test/fastReads');
  return withFastReads(await importOriginal());
});

function deferred() {
  let release!: () => void;
  const promise = new Promise<void>((resolve) => {
    release = resolve;
  });
  return { promise, release };
}

const REQUEST_ID = expect.stringMatching(/^ui-/);

function report(): TaskView['reports'][number] {
  return {
    task_id: 'task-1',
    attempt_id: 'attempt-1',
    request_id: 'report-1',
    summary: 'Checked the logs.',
    outcome: 'progress',
    owner_action: null,
    evidence_refs: []
  };
}

describe('task page layout', () => {
  it('leads with the actions, then only the sections that have something to say, with plain headings', async () => {
    server.use(...serveTaskPage(taskScreen(view())));
    renderTasks('/tasks/task-1');

    await screen.findByRole('heading', { level: 1, name: 'Add retry button' });
    const headings = screen.getAllByRole('heading', { level: 2 }).map((h) => h.textContent);
    // No operation, report or child task here, so those sections are not drawn at all.
    expect(headings).toEqual(['Actions', 'Workspace', 'Pull request', 'Attempts']);
    // No label on the page is wrapped in brackets.
    expect(screen.getByRole('article').textContent).not.toMatch(/\[[A-Z ·]+\]/);
    expect(screen.getByTestId('task-status')).toHaveTextContent(/^running$/i);
  });

  it('draws every section when the task has something for each', async () => {
    const full = view(
      {},
      {
        operations: [operation()],
        reports: [report()],
        projection: projection({ repository: 'acme/widgets', pr_state: 'open', pr_number: 7 })
      }
    );
    const child = view({ id: 'child-1', parent_task_id: 'task-1', title: 'Child' });
    server.use(...serveTaskPage(taskScreen(full, [child])));
    renderTasks('/tasks/task-1');

    await screen.findByRole('link', { name: /Child/ });
    expect(screen.getAllByRole('heading', { level: 2 }).map((h) => h.textContent)).toEqual([
      'Actions',
      'Workspace',
      'Pull request',
      'In progress',
      'Attempts',
      'Reports',
      'Child tasks'
    ]);
  });

  it('shows no workspace or pull request section on a task with no repository, branch or pull request', async () => {
    const ops = view({ checkout: null }, { attempts: [attempt({ session_id: null })] });
    server.use(...serveTaskPage(taskScreen(ops)));
    renderTasks('/tasks/task-1');

    await screen.findByRole('heading', { level: 1, name: 'Add retry button' });
    expect(screen.queryByTestId('task-workspace')).toBeNull();
    expect(screen.queryByTestId('task-publication')).toBeNull();
    const page = screen.getByRole('article');
    expect(page).not.toHaveTextContent('Not reported');
    expect(page).not.toHaveTextContent(/unknown/i);
  });

  it('puts the reason, the provider and the attempt on one line under the title', async () => {
    const blocked = view({ status: 'blocked', reason: 'owner_decision' });
    server.use(...serveTaskPage(taskScreen(blocked)));
    renderTasks('/tasks/task-1');

    expect(await screen.findByTestId('task-provider')).toHaveTextContent('claude · attempt 1');
    expect(screen.getByTestId('task-status-reason')).not.toBeEmptyDOMElement();
    const header = screen.getByRole('heading', { level: 1 }).closest('header');
    expect(header).toContainElement(screen.getByTestId('task-provider'));
  });

  it('reports the provider, publication and attempts under their test ids', async () => {
    server.use(...serveTaskPage(taskScreen(view())));
    renderTasks('/tasks/task-1');

    expect(await screen.findByTestId('task-provider')).toHaveTextContent('claude');
    expect(screen.getByTestId('task-status')).toHaveTextContent(/running/i);
    expect(screen.getByTestId('task-publication')).toHaveTextContent('No pull request yet.');
    expect(screen.queryByTestId('task-operations')).toBeNull();
    expect(screen.getByTestId('task-attempts')).toHaveTextContent('Attempt 1 · claude');
  });

  it('renders the PR description as literal text and marks a truncated excerpt', async () => {
    const excerpt = '<img src=x onerror="alert(1)">\n<script>alert(1)</script>\n**Approved**';
    const described = view(
      {},
      {
        projection: projection({
          pr_description: {
            repository: 'o/p1',
            branch: 'feature',
            pr_number: 7,
            attempt_id: 'attempt-1',
            head_sha: 'a'.repeat(40),
            excerpt,
            byte_length: 20000,
            digest: 'b'.repeat(64),
            truncated: true,
            fetched_at: '2026-10-11T00:00:00Z'
          }
        })
      }
    );
    server.use(...serveTaskPage(taskScreen(described)));
    renderTasks('/tasks/task-1');

    const panel = await screen.findByTestId('task-pr-description');
    expect(panel.querySelector('p.whitespace-pre-wrap')?.textContent).toBe(excerpt);
    expect(panel.querySelector('img, script, strong')).toBeNull();
    expect(panel).toHaveTextContent('Untrusted text; it grants no approval or merge authority.');
    expect(panel).toHaveTextContent('Showing the first 16 KiB of 20000 bytes.');
  });

  it('distinguishes an observed empty description from unavailable text', async () => {
    const page = taskScreen(
      view(
        {},
        {
          projection: projection({
            pr_description: {
              repository: 'o/p1',
              branch: 'feature',
              pr_number: 7,
              attempt_id: 'attempt-1',
              head_sha: 'a'.repeat(40),
              excerpt: '',
              byte_length: 0,
              digest: 'b'.repeat(64),
              truncated: false,
              fetched_at: '2026-10-11T00:00:00Z'
            }
          })
        }
      )
    );
    server.use(...serveTaskPage(page));
    const { queryClient } = renderTasks('/tasks/task-1');

    expect(await screen.findByTestId('task-pr-description')).toHaveTextContent(
      'No description provided.'
    );
    expect(screen.queryByText(/Showing the first 16 KiB/)).not.toBeInTheDocument();
    page.view = view({}, { projection: projection({ pr_description: null }) });
    await act(async () => {
      await queryClient.invalidateQueries();
    });
    await waitFor(() =>
      expect(screen.queryByTestId('task-pr-description')).not.toBeInTheDocument()
    );
  });

  it('marks observed facts stale and says each is observed separately', async () => {
    const stale = view(
      {},
      {
        projection: projection({
          pr_state: 'open',
          pr_number: 7,
          observed_at: '2020-01-01T00:00:00Z'
        })
      }
    );
    server.use(...serveTaskPage(taskScreen(stale)));
    renderTasks('/tasks/task-1');

    const observed = await screen.findByTestId('task-observed');
    expect(observed).toHaveTextContent('(stale)');
    expect(observed).toHaveTextContent('Each is observed separately; none implies the others.');
  });

  it('shows the unverified provider summary under its label', async () => {
    const summarised = view(
      {},
      {
        artifacts: [
          {
            id: 'artifact-1',
            operation_id: 'operation-done',
            kind: 'unverified_provider_summary',
            sha256: 'a'.repeat(64),
            payload: { summary: 'I finished the button.' }
          }
        ]
      }
    );
    server.use(...serveTaskPage(taskScreen(summarised)));
    renderTasks('/tasks/task-1');

    const section = await screen.findByTestId('task-unverified-summary');
    expect(within(section).getByText('I finished the button.')).toBeInTheDocument();
    expect(within(section).getByText(/has not verified it/)).toBeInTheDocument();
  });

  it('lists the pending owner actions and links to the Inbox', async () => {
    const waiting = view(
      { reason: 'approval' },
      { projection: projection({ pending_approval_ids: ['approval-1'] }) }
    );
    server.use(...serveTaskPage(taskScreen(waiting)));
    renderTasks('/tasks/task-1');

    const panel = await screen.findByTestId('task-pending-approvals');
    expect(within(panel).getByText('1 pending owner action')).toBeInTheDocument();
    expect(within(panel).getByRole('link', { name: 'Open Inbox' })).toHaveAttribute(
      'href',
      '/inbox?item=approval-1'
    );
  });

  it('says so when an approval reason has no pending request behind it', async () => {
    server.use(...serveTaskPage(taskScreen(view({ reason: 'approval' }))));
    renderTasks('/tasks/task-1');

    expect(
      await screen.findByText(/Waiting on owner approval, but the server lists no pending request/)
    ).toBeInTheDocument();
  });

  it('links a child to its parent and lists its children', async () => {
    const parent = view({ id: 'task-parent', title: 'Parent work' });
    const child = view({ id: 'task-child', title: 'Child work', parent_task_id: 'task-1' });
    const page = taskScreen(view({ parent_task_id: 'task-parent' }), [parent, child]);
    const filters: (string | null)[] = [];
    server.use(
      // The parent is read by its own id, so the read answers with the parent for that id.
      taskRead(({ params }) =>
        HttpResponse.json(params.task_id === 'task-parent' ? parent : page.view)
      ),
      tasksList(({ request }) => {
        filters.push(new URL(request.url).searchParams.get('parent_task_id'));
        return HttpResponse.json(page.list);
      }),
      providersRead(page.providers)
    );
    renderTasks('/tasks/task-1');

    expect(await screen.findByRole('link', { name: 'Parent work' })).toHaveAttribute(
      'href',
      '/tasks/task-parent'
    );
    expect(await screen.findByRole('link', { name: /Child work/ })).toHaveAttribute(
      'href',
      '/tasks/task-child'
    );
    // The children come from the list's parent filter; the page never asks for the whole list.
    expect(filters).toContain('task-1');
    expect(filters).not.toContain(null);
  });

  it('keeps every button a button and every target at the control height', async () => {
    server.use(...serveTaskPage(taskScreen(allowedView())));
    renderTasks('/tasks/task-1');
    await screen.findByTestId('task-action-retry');

    const buttons = screen.getAllByRole('button');
    expect(buttons.length).toBeGreaterThan(0);
    for (const button of buttons) {
      expect(button).toHaveAttribute('type', 'button');
      expect(button).toHaveClass('min-h-control');
    }
    for (const link of screen.getAllByRole('link')) {
      expect(link).toHaveClass('min-h-control');
    }
  });
});

describe('task page loading and failure', () => {
  it('reports a malformed payload without crashing', async () => {
    server.use(
      taskRead(() => HttpResponse.json({ task: { id: 'task-1' } })),
      tasksList(() => HttpResponse.json([])),
      providersRead()
    );
    renderTasks('/tasks/task-1');

    expect(
      await screen.findByText('Mainloop sent a task this page cannot read.')
    ).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Retry loading' })).toBeInTheDocument();
  });

  it('keeps the task on screen when the parent and child list cannot load', async () => {
    server.use(
      taskRead(() => HttpResponse.json(view())),
      tasksList(problem(500, 'list offline')),
      providersRead()
    );
    renderTasks('/tasks/task-1');

    expect(
      await screen.findByText('Parent and child tasks could not be loaded.')
    ).toBeInTheDocument();
    expect(screen.getByRole('heading', { level: 1, name: 'Add retry button' })).toBeInTheDocument();
    expect(screen.queryByText(/Showing the last known state/)).not.toBeInTheDocument();
  });

  it('shows the task when the list payload is not a list', async () => {
    server.use(
      taskRead(() => HttpResponse.json(view())),
      tasksList(() => HttpResponse.json({})),
      providersRead()
    );
    renderTasks('/tasks/task-1');

    expect(
      await screen.findByText('Parent and child tasks could not be loaded.')
    ).toBeInTheDocument();
    expect(screen.getByRole('heading', { level: 1, name: 'Add retry button' })).toBeInTheDocument();
  });

  it('shows the task when one of its attempts is malformed', async () => {
    server.use(
      taskRead(() => HttpResponse.json({ ...view(), attempts: [null] })),
      tasksList(() => HttpResponse.json([])),
      providersRead()
    );
    renderTasks('/tasks/task-1');

    expect(
      await screen.findByRole('heading', { level: 1, name: 'Add retry button' })
    ).toBeVisible();
    expect(
      screen.queryByText('Mainloop sent a task this page cannot read.')
    ).not.toBeInTheDocument();
  });

  it('shows a not-found page for a task the server does not have', async () => {
    server.use(
      taskRead(problem(404, 'Not Found')),
      tasksList(() => HttpResponse.json([])),
      providersRead()
    );
    renderTasks('/tasks/task-missing');

    expect(await screen.findByTestId('task-not-found')).toHaveTextContent('Task not found');
  });

  it('says the backend is unreachable when no response arrives', async () => {
    server.use(
      taskRead(() => HttpResponse.error()),
      tasksList(() => HttpResponse.json([])),
      providersRead()
    );
    renderTasks('/tasks/task-1');

    expect(await screen.findByText("Can't reach the Mainloop backend.")).toBeInTheDocument();
  });
});

describe('back links and finished tasks', () => {
  it('goes back to the parent task when the task has one', async () => {
    server.use(...serveTaskPage(taskScreen(view({ parent_task_id: 'task-parent' }))));
    renderTasks('/tasks/task-1');

    expect(await screen.findByRole('link', { name: 'Back to parent task' })).toHaveAttribute(
      'href',
      '/tasks/task-parent'
    );
  });

  it('goes back to the project when the task has no parent', async () => {
    server.use(...serveTaskPage(taskScreen(view({ project_id: 'project-1' }))));
    renderTasks('/tasks/task-1');

    expect(await screen.findByRole('link', { name: 'Back to project' })).toHaveAttribute(
      'href',
      '/projects/project-1'
    );
  });

  it('goes back to the sessions home for a task with no parent or project', async () => {
    server.use(...serveTaskPage(taskScreen(view({ project_id: null }))));
    renderTasks('/tasks/task-1');

    expect(await screen.findByRole('link', { name: 'Back' })).toHaveAttribute('href', '/');
  });

  it('a cancelled task with nothing open has no in-progress heading', async () => {
    server.use(...serveTaskPage(taskScreen(view({ status: 'cancelled' }))));
    renderTasks('/tasks/task-1');

    await screen.findByRole('heading', { level: 1, name: 'Add retry button' });
    expect(screen.queryByRole('heading', { name: 'In progress' })).not.toBeInTheDocument();
    expect(screen.queryByTestId('task-operations')).toBeNull();
  });
});

describe('a task that waits on publication', () => {
  const waiting = (
    facts: Parameters<typeof projection>[0],
    rest: Parameters<typeof view>[1] = {}
  ) =>
    view(
      { status: 'waiting', reason: 'publication' },
      {
        projection: projection(facts),
        actions: {
          retry: { available: true, reason: null },
          reassign: { available: false, reason: 'handoff_unavailable' },
          cancel: { available: true, reason: null }
        },
        ...rest
      }
    );

  it('says a green pull request needs the owner, how to land it, and does not lead with Retry', async () => {
    // Given a task whose pull request is open with CI passed and no merge proposed.
    const page = waiting({
      pr_state: 'open',
      ci_state: 'success',
      pr_number: 228,
      pr_url: 'https://github.com/acme/widgets/pull/228'
    });
    server.use(...serveTaskPage(taskScreen(page)));

    // When its page opens.
    renderTasks('/tasks/task-1');
    const banner = await screen.findByTestId('task-publication-ask');

    // Then it leads with what is needed and the two ways on.
    expect(banner).toHaveAttribute('data-kind', 'ready');
    expect(within(banner).getByRole('heading', { name: 'Needs you' })).toBeInTheDocument();
    expect(banner).toHaveTextContent(
      'PR #228 is open and CI passed. Nothing merges it on its own.'
    );
    expect(banner).toHaveTextContent(/follow-up to merge it/);
    expect(within(banner).getByRole('link', { name: 'Open PR #228 on GitHub' })).toHaveAttribute(
      'href',
      'https://github.com/acme/widgets/pull/228'
    );
    // And Retry is offered, but is not the button the page points at.
    const retry = screen.getByTestId('task-action-retry');
    expect(retry).toBeEnabled();
    expect(retry.className).not.toMatch(/bg-primary/);
    // The banner sits above the actions.
    expect(
      banner.compareDocumentPosition(screen.getByTestId('task-actions')) &
        Node.DOCUMENT_POSITION_FOLLOWING
    ).toBeTruthy();
  });

  it('says a merged pull request completes the task only when its own merge did it', async () => {
    server.use(
      ...serveTaskPage(
        taskScreen(waiting({ pr_state: 'merged', pr_number: 237, merge_state: 'merged' }))
      )
    );
    renderTasks('/tasks/task-1');

    const banner = await screen.findByTestId('task-publication-ask');
    expect(banner).toHaveAttribute('data-kind', 'merged');
    expect(
      within(banner).getByRole('heading', { name: 'Waiting on its pull request' })
    ).toBeInTheDocument();
    expect(banner).toHaveTextContent(/PR #237 is merged\./);
    expect(banner).toHaveTextContent(/cancel it to close it/);
  });

  it('says so when there is no pull request to wait for', async () => {
    server.use(...serveTaskPage(taskScreen(waiting({ pr_state: 'unknown', pr_number: null }))));
    renderTasks('/tasks/task-1');

    const banner = await screen.findByTestId('task-publication-ask');
    expect(banner).toHaveAttribute('data-kind', 'none');
    expect(banner).toHaveTextContent(/no pull request is linked to it/);
    expect(within(banner).queryByRole('link')).not.toBeInTheDocument();
  });

  it('says nothing while CI is still running or a merge is under way', async () => {
    server.use(
      ...serveTaskPage(taskScreen(waiting({ pr_state: 'open', ci_state: 'pending', pr_number: 5 })))
    );
    renderTasks('/tasks/task-1');

    await screen.findByTestId('task-actions');
    expect(screen.queryByTestId('task-publication-ask')).not.toBeInTheDocument();
  });
});

describe('which action leads', () => {
  it('leads with Retry on a task that has stopped', async () => {
    const failed = view(
      { status: 'failed' },
      { actions: { ...view().actions, retry: { available: true, reason: null } } }
    );
    server.use(...serveTaskPage(taskScreen(failed)));
    renderTasks('/tasks/task-1');

    expect((await screen.findByTestId('task-action-retry')).className).toMatch(/bg-primary/);
  });

  it('offers Retry quietly on a task that is still running', async () => {
    const running = view(
      { status: 'running' },
      { actions: { ...view().actions, retry: { available: true, reason: null } } }
    );
    server.use(...serveTaskPage(taskScreen(running)));
    renderTasks('/tasks/task-1');

    const retry = await screen.findByTestId('task-action-retry');
    expect(retry).toBeEnabled();
    expect(retry.className).not.toMatch(/bg-primary/);
  });
});

describe('an operation that stopped part-way', () => {
  it('says it is on hold, and marks each step so its state is not colour alone', async () => {
    // Given a failed task whose handoff is blocked after creating the new attempt.
    const held = view(
      { status: 'failed' },
      {
        operations: [
          operation({
            kind: 'create',
            state: 'blocked',
            last_confirmed_step: 'target_creating',
            reason: 'provisioning_unavailable'
          })
        ]
      }
    );
    server.use(...serveTaskPage(taskScreen(held)));

    // When its page opens and the details are shown.
    renderTasks('/tasks/task-1');
    const panel = await screen.findByTestId('task-operations');

    // Then the section is not called in progress, and the summary reads as a sentence.
    expect(within(panel).getByRole('heading', { name: 'On hold' })).toBeInTheDocument();
    expect(panel).toHaveTextContent(
      'Create is blocked after “Creating the new attempt”: workspace provisioning is not available yet'
    );
    // And the steps are marked and kept apart.
    const steps = within(within(panel).getByTestId('task-operation-steps')).getAllByRole(
      'listitem'
    );
    expect(steps.map((step) => step.getAttribute('data-status'))).toEqual([
      'done',
      'done',
      'done',
      'done',
      'done',
      'done',
      'done',
      'pending',
      'pending'
    ]);
    expect(steps[0]).toHaveTextContent('Done: Requested');
    expect(steps[6]).toHaveTextContent('› ✓ Done: Creating the new attempt');
    expect(steps[7]).toHaveTextContent('› New attempt ready');
  });

  it('says in progress while an operation is still moving', async () => {
    const moving = view(
      {},
      {
        operations: [operation({ state: 'draining', last_confirmed_step: 'requested' })]
      }
    );
    server.use(...serveTaskPage(taskScreen(moving)));
    renderTasks('/tasks/task-1');

    const panel = await screen.findByTestId('task-operations');
    expect(within(panel).getByRole('heading', { name: 'In progress' })).toBeInTheDocument();
    expect(
      within(panel).getByText(/Draining the current attempt/, { selector: 'li' })
    ).toHaveAttribute('aria-current', 'step');
  });
});

describe('owner actions', () => {
  it('hides retry and reassign while the handoff is not installed, and says nothing about why', async () => {
    const page = view(
      {},
      {
        actions: {
          ...view().actions,
          retry: { available: false, reason: 'handoff_unavailable' },
          reassign: { available: false, reason: 'handoff_unavailable' }
        }
      }
    );
    server.use(...serveTaskPage(taskScreen(page)));
    renderTasks('/tasks/task-1');

    // Cancel is still offered, so only the dead controls go.
    expect(await screen.findByTestId('task-action-cancel')).toBeEnabled();
    expect(screen.queryByTestId('task-action-retry')).not.toBeInTheDocument();
    expect(screen.queryByTestId('task-action-reassign')).not.toBeInTheDocument();
    // How the server is built is not the owner's concern: nothing on the page names the handoff.
    expect(screen.getByRole('article')).not.toHaveTextContent(/handoff/i);
  });

  it('offers no actions on a task that has ended', async () => {
    // Given a completed task: the server refuses every action, cancel included.
    const ended = { available: false, reason: 'cancel_unavailable' } as const;
    const none = { available: false, reason: 'handoff_unavailable' } as const;
    const page = view(
      { status: 'completed' },
      { actions: { retry: none, reassign: none, cancel: ended } }
    );
    server.use(...serveTaskPage(taskScreen(page)));

    // When its page opens.
    renderTasks('/tasks/task-1');
    await screen.findByRole('heading', { level: 1, name: 'Add retry button' });

    // Then there is no Cancel button, no reason for one, and no empty actions panel.
    expect(screen.queryByTestId('task-action-cancel')).not.toBeInTheDocument();
    expect(screen.queryByTestId('task-actions')).not.toBeInTheDocument();
    expect(screen.getByRole('article')).not.toHaveTextContent(/cancel/i);
  });

  it('keeps Retry and drops Cancel on a failed task', async () => {
    // Given a failed task the server still lets the owner retry.
    const page = view(
      { status: 'failed' },
      {
        actions: {
          retry: { available: true, reason: null },
          reassign: { available: false, reason: 'handoff_unavailable' },
          cancel: { available: false, reason: 'cancel_unavailable' }
        }
      }
    );
    server.use(...serveTaskPage(taskScreen(page)));

    // When its page opens.
    renderTasks('/tasks/task-1');

    // Then Retry is the action, and nothing offers or explains a cancel.
    expect(await screen.findByTestId('task-action-retry')).toBeEnabled();
    expect(screen.queryByTestId('task-action-cancel')).not.toBeInTheDocument();
    expect(screen.getByTestId('task-actions')).not.toHaveTextContent(/cancel/i);
  });

  it('hides only the action the handoff makes unavailable', async () => {
    const page = view(
      {},
      {
        actions: {
          ...view().actions,
          retry: { available: false, reason: 'handoff_unavailable' }
        }
      }
    );
    server.use(...serveTaskPage(taskScreen(page)));
    renderTasks('/tasks/task-1');

    // Reassign is still offered.
    expect(await screen.findByTestId('task-action-reassign')).toBeInTheDocument();
    expect(screen.queryByTestId('task-action-retry')).not.toBeInTheDocument();
    expect(screen.getByRole('article')).not.toHaveTextContent(/handoff/i);
  });

  it('keeps a disabled action with its server reason when the reason is not the handoff', async () => {
    const page = view(
      {},
      {
        actions: {
          ...view().actions,
          retry: { available: false, reason: 'provisioning_unavailable' }
        }
      }
    );
    server.use(...serveTaskPage(taskScreen(page)));
    renderTasks('/tasks/task-1');

    expect(await screen.findByTestId('task-action-retry')).toBeDisabled();
    // The reason is named, so it reads with its button though the buttons share a row.
    expect(
      screen.getByText('Retry: workspace provisioning is not available yet')
    ).toBeInTheDocument();
  });

  it('asks twice before cancelling, and sends the version and attempt the owner saw', async () => {
    const bodies: unknown[] = [];
    server.use(
      ...serveTaskPage(taskScreen(view())),
      taskAction('cancel', async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json(operation({ kind: 'cancel' }));
      })
    );
    const user = userEvent.setup();
    renderTasks('/tasks/task-1');

    await user.click(await screen.findByTestId('task-action-cancel'));
    const dialog = await screen.findByRole('alertdialog');
    expect(within(dialog).getByText('Cancel this task?')).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Keep running' }));
    await waitFor(() => expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument());
    expect(bodies).toEqual([]);

    await user.click(screen.getByTestId('task-action-cancel'));
    const again = await screen.findByRole('alertdialog');
    await user.click(within(again).getByRole('button', { name: 'Confirm cancel' }));

    expect(
      await screen.findByText('Cancel requested. Progress shows below as the server reports it.')
    ).toBeInTheDocument();
    expect(bodies).toEqual([
      { request_id: REQUEST_ID, expected_version: 3, expected_attempt_id: 'attempt-1' }
    ]);
  });

  it('closes the cancel dialog and sends nothing when the task moves on while it is open', async () => {
    const bodies: unknown[] = [];
    const page = taskScreen(view({ version: 3 }));
    server.use(
      ...serveTaskPage(page),
      taskAction('cancel', async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json(operation({ kind: 'cancel' }));
      })
    );
    const user = userEvent.setup();
    const { queryClient } = renderTasks('/tasks/task-1');

    await user.click(await screen.findByTestId('task-action-cancel'));
    await screen.findByRole('alertdialog');

    // Someone else moves the task on while the owner is still deciding.
    page.view = view({ version: 4 });
    const apply = createSseApplier(queryClient);
    apply({ event: 'connected', data: '' });
    apply({
      event: 'task:updated',
      data: JSON.stringify({ event_id: 'cancel-moved-1', task_id: 'task-1', version: 4 })
    });

    await waitFor(() => expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument());
    expect(
      screen.getByText(
        'The task changed before you confirmed, so nothing was cancelled. Check it, then cancel again.'
      )
    ).toBeInTheDocument();
    expect(bodies).toEqual([]);
  });

  it('keeps the status line after the task moves on, and cancels the new version when asked again', async () => {
    const bodies: unknown[] = [];
    const page = taskScreen(view({ version: 3 }));
    server.use(
      ...serveTaskPage(page),
      taskAction('cancel', async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json(operation({ kind: 'cancel' }));
      })
    );
    const user = userEvent.setup();
    const { queryClient } = renderTasks('/tasks/task-1');
    const status =
      'The task changed before you confirmed, so nothing was cancelled. Check it, then cancel again.';

    await user.click(await screen.findByTestId('task-action-cancel'));
    await screen.findByRole('alertdialog');
    page.view = view({ version: 4 });
    const apply = createSseApplier(queryClient);
    apply({ event: 'connected', data: '' });
    apply({
      event: 'task:updated',
      data: JSON.stringify({ event_id: 'cancel-moved-2', task_id: 'task-1', version: 4 })
    });

    // The dialog closes, and the status line stays until the owner acts again.
    await waitFor(() => expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument());
    expect(screen.getByText(status)).toBeInTheDocument();

    await user.click(screen.getByTestId('task-action-cancel'));
    expect(screen.queryByText(status)).not.toBeInTheDocument();
    await user.click(
      within(await screen.findByRole('alertdialog')).getByRole('button', { name: 'Confirm cancel' })
    );

    await waitFor(() =>
      expect(bodies).toEqual([
        { request_id: REQUEST_ID, expected_version: 4, expected_attempt_id: 'attempt-1' }
      ])
    );
  });

  it('shows not-found when the re-read after an action finds the task gone', async () => {
    let gone = false;
    server.use(
      taskRead(() =>
        gone
          ? HttpResponse.json({ detail: 'Not Found' }, { status: 404 })
          : HttpResponse.json(allowedView())
      ),
      tasksList(() => HttpResponse.json([])),
      providersRead(),
      taskAction('retry', () => {
        gone = true;
        return HttpResponse.json(operation({ kind: 'retry' }));
      })
    );
    const user = userEvent.setup();
    renderTasks('/tasks/task-1');

    await user.click(await screen.findByTestId('task-action-retry'));

    expect(await screen.findByTestId('task-not-found')).toHaveTextContent('Task not found');
    expect(screen.queryByRole('heading', { name: 'Add retry button' })).not.toBeInTheDocument();
  });

  it('reassigns to the other provider with the target the owner saw', async () => {
    const bodies: unknown[] = [];
    server.use(
      ...serveTaskPage(taskScreen(view())),
      taskAction('reassign', async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json(operation({ kind: 'reassign' }));
      })
    );
    const user = userEvent.setup();
    renderTasks('/tasks/task-1');

    const picker = await screen.findByLabelText('Provider to reassign to');
    expect(
      await within(picker).findByRole('option', { name: 'Codex (codex)' })
    ).toBeInTheDocument();
    await user.click(screen.getByTestId('task-action-reassign'));

    expect(
      await screen.findByText('Reassign requested. Progress shows below as the server reports it.')
    ).toBeInTheDocument();
    expect(bodies).toEqual([
      {
        request_id: REQUEST_ID,
        expected_version: 3,
        expected_attempt_id: 'attempt-1',
        target_profile_id: 'codex-default'
      }
    ]);
  });

  it('sends one request when the owner presses twice', async () => {
    const gate = deferred();
    let calls = 0;
    server.use(
      ...serveTaskPage(taskScreen(view())),
      taskAction('reassign', async () => {
        calls += 1;
        await gate.promise;
        return HttpResponse.json(operation({ kind: 'reassign' }));
      })
    );
    const user = userEvent.setup();
    renderTasks('/tasks/task-1');

    const reassign = await screen.findByTestId('task-action-reassign');
    await waitFor(() => expect(reassign).toBeEnabled());
    await user.click(reassign);
    await user.click(reassign);
    expect(reassign).toBeDisabled();
    expect(calls).toBe(1);

    gate.release();
    expect(
      await screen.findByText('Reassign requested. Progress shows below as the server reports it.')
    ).toBeInTheDocument();
    expect(calls).toBe(1);
  });

  it('clears the notice when the owner moves to another task', async () => {
    const other = view({ id: 'task-2', title: 'Other task' });
    server.use(
      taskRead(({ params }) =>
        HttpResponse.json(params.task_id === 'task-2' ? other : allowedView())
      ),
      tasksList(() => HttpResponse.json([])),
      providersRead(),
      taskAction('cancel', () => HttpResponse.json(operation({ kind: 'cancel' })))
    );
    const user = userEvent.setup();
    const { router } = renderTasks('/tasks/task-1');

    await user.click(await screen.findByTestId('task-action-cancel'));
    await user.click(
      within(await screen.findByRole('alertdialog')).getByRole('button', { name: 'Confirm cancel' })
    );
    expect(
      await screen.findByText('Cancel requested. Progress shows below as the server reports it.')
    ).toBeInTheDocument();

    await act(() => router.navigate({ to: '/tasks/$id', params: { id: 'task-2' } }));

    expect(
      await screen.findByRole('heading', { level: 1, name: 'Other task' })
    ).toBeInTheDocument();
    expect(screen.queryByTestId('task-action-notice')).not.toBeInTheDocument();
  });

  it('re-reads the task on a 409 before it says the task changed', async () => {
    const page = taskScreen(view());
    server.use(
      ...serveTaskPage(page),
      taskAction('reassign', (info) => {
        page.view = view({ title: 'Renamed by someone else', version: 4 });
        return taskProblem(409, 'stale_task_version')(info);
      })
    );
    const user = userEvent.setup();
    renderTasks('/tasks/task-1');

    await user.click(await screen.findByTestId('task-action-reassign'));

    expect(
      await screen.findByText(
        'The task changed since you loaded it. Review the latest state and try again.'
      )
    ).toBeInTheDocument();
    expect(screen.getByTestId('task-action-notice')).toHaveTextContent(
      'Server reason: stale_task_version'
    );
    // The notice appears only after the re-read: the page already shows the new title.
    expect(page.reads).toBe(2);
    expect(
      screen.getByRole('heading', { level: 1, name: 'Renamed by someone else' })
    ).toBeInTheDocument();
  });

  it('shows the backend reason in words and quotes its code', async () => {
    server.use(
      ...serveTaskPage(taskScreen(view())),
      taskAction('reassign', taskProblem(409, 'parent_capacity'))
    );
    const user = userEvent.setup();
    renderTasks('/tasks/task-1');

    await user.click(await screen.findByTestId('task-action-reassign'));

    expect(
      await screen.findByText('The parent task has no capacity for another attempt right now.')
    ).toBeInTheDocument();
    expect(screen.getByTestId('task-action-notice')).toHaveTextContent(
      'Server reason: parent_capacity'
    );
  });

  // Last in the file: the connection store is a module singleton and stays offline afterwards.
  it('disables the actions and says so while the connection is down', async () => {
    server.use(...serveTaskPage(taskScreen(allowedView())));
    renderTasks('/tasks/task-1');
    expect(await screen.findByTestId('task-action-retry')).toBeEnabled();

    // Nothing is probed here: the store is only told the connection failed, and it is never restarted.
    act(() => connection.reportFailure());

    expect(await screen.findAllByText('Mainloop is offline.')).not.toHaveLength(0);
    expect(screen.getByTestId('task-action-retry')).toBeDisabled();
    expect(screen.getByTestId('task-action-cancel')).toBeDisabled();
  });
});

describe('what leads a task that waits on the owner', () => {
  const blocked = (rest: Parameters<typeof view>[1] = {}) =>
    view(
      {
        id: 'task-abcdef123456',
        title: 'ops: check the logs',
        status: 'blocked',
        reason: 'owner_decision',
        owner_decision: { attempt_id: 'attempt-1', report_id: 'report-1', action: 'Fix the token.' }
      },
      {
        actions: {
          retry: { available: false, reason: 'handoff_unavailable' },
          reassign: { available: false, reason: 'handoff_unavailable' },
          cancel: { available: true, reason: null }
        },
        ...rest
      }
    );

  it('offers the ways to answer first, and Cancel last and not as the red primary', async () => {
    server.use(...serveTaskPage(taskScreen(blocked())));
    renderTasks('/tasks/task-abcdef123456');

    const actions = await screen.findByTestId('task-actions');
    const labels = [...actions.querySelectorAll('button, a')].map((control) => control.textContent);
    expect(labels).toEqual(['I fixed it, retry', 'Answer in chat', 'Open session', 'Cancel task']);
    // Cancel turns red only in its confirmation; here it is an outline button with red text.
    expect(screen.getByTestId('task-action-cancel').className).not.toMatch(/bg-destructive/);
    // Nothing internal: no version or attempt bookkeeping, and no word about the handoff.
    expect(screen.getByRole('article')).not.toHaveTextContent(/Acting on version|handoff/i);
  });

  it('opens the main chat with the message filled in, not sent', async () => {
    const user = userEvent.setup();
    server.use(...serveTaskPage(taskScreen(blocked())));
    const { router } = renderTasks('/tasks/task-abcdef123456');

    await user.click(await screen.findByRole('button', { name: 'I fixed it, retry' }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/'));
    expect(router.state.location.search).toEqual({
      prefill: 'About task ops: check the logs (task-abc): I fixed it. Retry this task.'
    });
  });

  it('starts a blank message about the task from Answer in chat', async () => {
    const user = userEvent.setup();
    server.use(...serveTaskPage(taskScreen(blocked())));
    const { router } = renderTasks('/tasks/task-abcdef123456');

    await user.click(await screen.findByRole('button', { name: 'Answer in chat' }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/'));
    expect(router.state.location.search).toEqual({
      prefill: 'About task ops: check the logs (task-abc): '
    });
  });

  it('leads with the server Retry when it is available, and does not also offer the chat retry', async () => {
    const page = blocked({
      actions: {
        retry: { available: true, reason: null },
        reassign: { available: false, reason: 'handoff_unavailable' },
        cancel: { available: true, reason: null }
      }
    });
    server.use(...serveTaskPage(taskScreen(page)));
    renderTasks('/tasks/task-abcdef123456');

    expect(await screen.findByTestId('task-action-retry')).toBeEnabled();
    expect(screen.queryByRole('button', { name: 'I fixed it, retry' })).toBeNull();
    expect(screen.getByRole('button', { name: 'Answer in chat' })).toBeInTheDocument();
  });

  it('offers no chat answers on a task that is just running', async () => {
    server.use(...serveTaskPage(taskScreen(view())));
    renderTasks('/tasks/task-1');

    await screen.findByTestId('task-action-cancel');
    expect(screen.queryByRole('button', { name: 'I fixed it, retry' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Answer in chat' })).toBeNull();
  });
});

describe('the brief', () => {
  it('shows a short brief whole, with nothing to open', async () => {
    server.use(...serveTaskPage(taskScreen(view({ brief: 'Add a retry button.' }))));
    renderTasks('/tasks/task-1');

    const brief = await screen.findByTestId('task-brief');
    expect(brief).toHaveTextContent('Add a retry button.');
    expect(within(brief).queryByRole('button')).toBeNull();
  });

  it('cuts a long brief to two lines until the owner opens it', async () => {
    const user = userEvent.setup();
    const long = 'Read the logs and report what each tool returned. '.repeat(8);
    server.use(...serveTaskPage(taskScreen(view({ brief: long }))));
    renderTasks('/tasks/task-1');

    const brief = await screen.findByTestId('task-brief');
    const text = brief.querySelector('p');
    expect(text).toHaveClass('line-clamp-2');
    const toggle = within(brief).getByRole('button', { name: 'Show the whole brief' });
    expect(toggle).toHaveAttribute('aria-expanded', 'false');
    await user.click(toggle);
    expect(text).not.toHaveClass('line-clamp-2');
    expect(within(brief).getByRole('button', { name: 'Show less of the brief' })).toHaveAttribute(
      'aria-expanded',
      'true'
    );
  });
});

describe('owner decision banner', () => {
  it('leads with the one-line ask and the actions, with the rest collapsed behind Task details', async () => {
    const page = view({
      status: 'blocked',
      reason: 'owner_decision',
      owner_decision: {
        attempt_id: 'attempt-1',
        report_id: 'report-1',
        action: 'Pick a region: eu or us?'
      }
    });
    server.use(...serveTaskPage(taskScreen(page)));
    renderTasks('/tasks/task-1');

    expect(await screen.findByTestId('task-owner-ask')).toHaveTextContent(
      'Pick a region: eu or us?'
    );
    // The actions render right away, not after a page of facts and history.
    expect(screen.getByTestId('task-action-cancel')).toBeInTheDocument();
    // Everything else sits behind a collapsed disclosure.
    const details = screen.getByText('Task details').closest('details');
    expect(details).not.toBeNull();
    expect(details).not.toHaveAttribute('open');
    expect(within(details as HTMLElement).getByTestId('task-attempts')).toBeInTheDocument();
    // The ask comes before the actions, and the actions before the details.
    const order = [
      screen.getByTestId('task-owner-ask'),
      screen.getByTestId('task-actions'),
      details as HTMLElement
    ];
    for (const [index, element] of order.entries()) {
      const next = order[index + 1];
      if (next) {
        expect(
          element.compareDocumentPosition(next) & Node.DOCUMENT_POSITION_FOLLOWING
        ).toBeTruthy();
      }
    }
  });

  it('keeps the plain layout, with no banner or collapsed details, without an owner decision', async () => {
    server.use(...serveTaskPage(taskScreen(view())));
    renderTasks('/tasks/task-1');

    await screen.findByTestId('task-provider');
    expect(screen.queryByTestId('task-owner-ask')).not.toBeInTheDocument();
    expect(screen.queryByText('Task details')).not.toBeInTheDocument();
  });
});

describe('the held view survives older and malformed reads', () => {
  /** Waits until no read is in flight, so a read an SSE event started has settled before it asserts. */
  async function settle(queryClient: QueryClient) {
    await waitFor(() => expect(queryClient.isFetching()).toBe(0));
  }

  const heading = (name: string) => screen.getByRole('heading', { level: 1, name });

  it('keeps the newer view when an older read lands after it', async () => {
    const page = taskScreen(view({ version: 5 }));
    server.use(...serveTaskPage(page));
    const { queryClient } = renderTasks('/tasks/task-1');
    await screen.findByRole('heading', { level: 1, name: 'Add retry button' });

    // The next read answers with a version older than the one the owner holds.
    page.view = view({ version: 4, title: 'Stale title' });
    const apply = createSseApplier(queryClient);
    apply({ event: 'connected', data: '' });
    apply({
      event: 'task:updated',
      data: JSON.stringify({ event_id: 'newer-1', task_id: 'task-1', version: 6 })
    });

    await waitFor(() => expect(page.reads).toBe(2));
    await settle(queryClient);
    expect(heading('Add retry button')).toBeInTheDocument();
    expect(screen.queryByText('Stale title')).not.toBeInTheDocument();
    expect(screen.getByRole('article')).toHaveAttribute('data-task-version', '5');
  });

  it('makes no read for a task update older than the view held', async () => {
    const page = taskScreen(view({ version: 5 }));
    server.use(...serveTaskPage(page));
    const { queryClient } = renderTasks('/tasks/task-1');
    await screen.findByRole('heading', { level: 1, name: 'Add retry button' });

    const apply = createSseApplier(queryClient);
    apply({ event: 'connected', data: '' });
    apply({
      event: 'task:updated',
      data: JSON.stringify({ event_id: 'older-1', task_id: 'task-1', version: 4 })
    });
    await settle(queryClient);
    expect(page.reads).toBe(1);

    // The same applier does read for a newer update, so the check above is not vacuous.
    apply({
      event: 'task:updated',
      data: JSON.stringify({ event_id: 'newer-2', task_id: 'task-1', version: 6 })
    });
    await waitFor(() => expect(page.reads).toBe(2));
  });

  it('keeps the last good view when a newer update brings a malformed task', async () => {
    let broken = false;
    const page = taskScreen(view({ version: 5 }));
    server.use(
      taskRead(() =>
        broken ? HttpResponse.json({ task: { id: 'task-1' } }) : HttpResponse.json(page.view)
      ),
      tasksList(() => HttpResponse.json([])),
      providersRead()
    );
    const { queryClient } = renderTasks('/tasks/task-1');
    await screen.findByRole('heading', { level: 1, name: 'Add retry button' });

    broken = true;
    const apply = createSseApplier(queryClient);
    apply({ event: 'connected', data: '' });
    apply({
      event: 'task:updated',
      data: JSON.stringify({ event_id: 'bad-1', task_id: 'task-1', version: 6 })
    });

    expect(await screen.findByText(/Showing the last known state/)).toBeInTheDocument();
    expect(heading('Add retry button')).toBeInTheDocument();
    expect(
      screen.queryByText('Mainloop sent a task this page cannot read.')
    ).not.toBeInTheDocument();
  });
});

describe('task publication and session ID', () => {
  it('says No pull request yet while the task has none', async () => {
    server.use(...serveTaskPage(taskScreen(view())));
    renderTasks('/tasks/task-1');

    await screen.findByRole('heading', { level: 1, name: 'Add retry button' });
    expect(screen.getByText('No pull request yet.')).toBeInTheDocument();
  });

  it('drops the "yet" once the task has finished without a pull request', async () => {
    server.use(...serveTaskPage(taskScreen(view({ status: 'completed' }))));
    renderTasks('/tasks/task-1');

    await screen.findByRole('heading', { level: 1, name: 'Add retry button' });
    expect(screen.getByTestId('task-publication')).toHaveTextContent(/^No pull request\.$/);
  });

  it('drops the chip once there is a pull request', async () => {
    server.use(
      ...serveTaskPage(
        taskScreen(
          view(
            {},
            {
              projection: projection({
                pr_number: 4,
                pr_state: 'open',
                pr_url: 'https://github.com/acme/widgets/pull/4'
              })
            }
          )
        )
      )
    );
    renderTasks('/tasks/task-1');

    await screen.findByRole('heading', { level: 1, name: 'Add retry button' });
    expect(screen.queryByText('Not published yet')).not.toBeInTheDocument();
    expect(screen.getByText('#4 open')).toBeInTheDocument();
  });

  it('shows the session ID as 8 characters with a copy button, not the full ID', async () => {
    server.use(...serveTaskPage(taskScreen(view())));
    renderTasks('/tasks/task-1');

    await screen.findByRole('heading', { level: 1, name: 'Add retry button' });
    const section = screen.getByRole('heading', { level: 2, name: 'Workspace' }).parentElement;
    expect(section).not.toBeNull();
    const scoped = within(section as HTMLElement);
    expect(scoped.getByText('session-')).toBeInTheDocument();
    expect(scoped.getByRole('button', { name: 'Copy full session ID' })).toBeInTheDocument();
    expect(scoped.queryByText('session-1')).toBeNull();
  });
});

describe('task attempt session link', () => {
  it('links the attempt session by its full ID, shows 8 characters with a copy button', async () => {
    server.use(...serveTaskPage(taskScreen(view())));
    renderTasks('/tasks/task-1');

    await screen.findByRole('heading', { level: 1, name: 'Add retry button' });
    const attempts = screen.getByTestId('task-attempts');
    const link = within(attempts).getByRole('link', { name: 'session' });
    expect(link).toHaveAttribute('href', '/sessions/session-1');
    expect(within(attempts).getByText('session-')).toBeInTheDocument();
    expect(
      within(attempts).getByRole('button', { name: 'Copy full attempt session ID' })
    ).toBeInTheDocument();
    expect(within(attempts).queryByText('session-1')).toBeNull();
  });
});
