import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { delay, HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { connection, keys } from '@/api';
import { createSseApplier } from '@/api/invalidation';
import { workspaceFixture } from '@/test/fixtures';
import { attempt, view } from '@/test/fixtures/tasks';
import { server } from '@/test/msw/server';
import { UNCONFIRMED, UNREADABLE, UNREADABLE_ACTION } from './model';
import {
  portsRead,
  problem,
  serveWorkspace,
  tasksRead,
  workspaceAction,
  workspaceDelete,
  workspaceRead
} from './test/handlers';
import { renderWorkspace } from './test/render';

// The page's reads would otherwise queue behind the app's read limiter, so an action's re-read
// would take seconds. See fastReads.ts for why this mock sits in each test file.
vi.mock('@/api/middleware', async (importOriginal) => {
  const { withFastReads } = await import('@/features/sessions/test/fastReads');
  return withFastReads(await importOriginal());
});

afterEach(async () => {
  vi.restoreAllMocks();
  await connection.check();
});

const heading = () => screen.findByRole('heading', { level: 1, name: 'Workspace' });
const button = (name: string) => screen.getByRole('button', { name });
/** The value beside a manifest label. */
const fact = (label: string) =>
  screen.getByText(label, { selector: 'dt' }).nextElementSibling as HTMLElement;

const environment = (image: string) =>
  workspaceFixture({
    manifest: {
      ...workspaceFixture().manifest,
      development_environment: {
        environment_id: 'env-1',
        version_id: 'version-1',
        image,
        platform: 'linux/amd64',
        policy_identity: 'policy-1'
      }
    }
  });

describe('workspace page', () => {
  it('shows the lifecycle, publication and manifest facts', async () => {
    server.use(
      ...serveWorkspace(
        workspaceFixture({
          observed_state: 'running',
          manifest: {
            ...workspaceFixture().manifest,
            ref: '',
            dev: { ports: [], idle_timeout_minutes: 45 }
          }
        })
      )
    );
    renderWorkspace('/workspaces/w1');
    await heading();
    expect(screen.getByTestId('workspace-lifecycle-badge')).toHaveTextContent(/WS RUNNING/i);
    expect(screen.getByTestId('workspace-publication-badge')).toHaveTextContent(
      'Read only · disabled'
    );
    expect(fact('Repository').querySelector('a')).toHaveAttribute(
      'href',
      'https://example.test/repo.git'
    );
    expect(fact('Repository').querySelector('a')).toHaveAttribute('rel', 'noopener noreferrer');
    expect(fact('Branch')).toHaveTextContent('main');
    expect(fact('Base ref')).toHaveTextContent('Default');
    expect(fact('Agent')).toHaveTextContent('codex');
    expect(fact('Idle timeout')).toHaveTextContent('45 minutes');
    expect(fact('Last activity')).toHaveTextContent('Not recorded');
    expect(fact('Preview ports')).toHaveTextContent('None declared');
    expect(fact('Development environment')).toHaveTextContent('Not declared');
  });

  it('shows the digest prefix of a pinned image', async () => {
    server.use(
      ...serveWorkspace(environment(`registry.example/mainloop-dev@sha256:0123456789abcdef0123`))
    );
    renderWorkspace('/workspaces/w1');
    await heading();
    expect(fact('Development environment')).toHaveTextContent('sha256:0123456789ab · linux/amd64');
  });

  it('shows "unknown" for a malformed digest, and the page still renders', async () => {
    server.use(...serveWorkspace(environment('registry.example/mainloop-dev@sha256:NOTHEX')));
    renderWorkspace('/workspaces/w1');
    await heading();
    expect(fact('Development environment')).toHaveTextContent('unknown · linux/amd64');
    expect(button('Suspend workspace')).toBeInTheDocument();
  });

  it('offers suspend only while running', async () => {
    server.use(...serveWorkspace(workspaceFixture({ observed_state: 'running' })));
    renderWorkspace('/workspaces/w1');
    await heading();
    expect(button('Suspend workspace')).toBeEnabled();
    expect(button('Resume workspace')).toBeDisabled();
    expect(button('Refresh status')).toBeEnabled();
    expect(button('Delete workspace')).toBeEnabled();
  });

  it('offers resume only while suspended', async () => {
    server.use(...serveWorkspace(workspaceFixture({ observed_state: 'suspended' })));
    renderWorkspace('/workspaces/w1');
    await heading();
    expect(button('Resume workspace')).toBeEnabled();
    expect(button('Suspend workspace')).toBeDisabled();
  });

  it('sends one suspend request for a double press', async () => {
    let suspends = 0;
    server.use(
      ...serveWorkspace(),
      workspaceAction('suspend', async () => {
        suspends += 1;
        await delay(100);
        return HttpResponse.json(workspaceFixture({ observed_state: 'suspending' }));
      })
    );
    renderWorkspace('/workspaces/w1');
    await heading();
    const suspend = button('Suspend workspace');
    fireEvent.click(suspend);
    fireEvent.click(suspend);
    await waitFor(() => expect(button('Refresh status')).toBeEnabled());
    expect(suspends).toBe(1);
  });

  it('asks before deleting, and cancelling sends nothing', async () => {
    let deletes = 0;
    server.use(
      ...serveWorkspace(),
      workspaceDelete(() => {
        deletes += 1;
        return new HttpResponse(null, { status: 204 });
      })
    );
    const user = userEvent.setup();
    renderWorkspace('/workspaces/w1');
    await heading();
    await user.click(button('Delete workspace'));
    const dialog = await screen.findByRole('alertdialog');
    expect(
      within(dialog).getByText(
        'Delete this workspace? Its checkout and any uncommitted work are removed.'
      )
    ).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Cancel' }));
    await waitFor(() => expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument());
    expect(deletes).toBe(0);
  });

  it('names a branch workspace as one in the delete prompt', async () => {
    server.use(
      ...serveWorkspace(workspaceFixture({ publication_mode: 'branch', publication_reason: null }))
    );
    const user = userEvent.setup();
    renderWorkspace('/workspaces/w1');
    await heading();
    await user.click(button('Delete workspace'));
    const dialog = await screen.findByRole('alertdialog');
    expect(
      within(dialog).getByText(
        'Delete this branch workspace? Its checkout and any uncommitted work are removed.'
      )
    ).toBeInTheDocument();
  });

  it('navigates to the sessions list once the delete succeeds', async () => {
    server.use(
      ...serveWorkspace(),
      workspaceDelete(() => new HttpResponse(null, { status: 204 }))
    );
    const user = userEvent.setup();
    const { router } = renderWorkspace('/workspaces/w1');
    await heading();
    await user.click(button('Delete workspace'));
    const dialog = await screen.findByRole('alertdialog');
    await user.click(within(dialog).getByRole('button', { name: 'Delete workspace' }));
    expect(await screen.findByText('Sessions list')).toBeInTheDocument();
    expect(router.state.location.pathname).toBe('/');
  });

  it('shows the backend reason for a 409, and re-reads before the controls come back', async () => {
    let reads = 0;
    server.use(
      workspaceRead(() => {
        reads += 1;
        return HttpResponse.json(workspaceFixture());
      }),
      portsRead(() => HttpResponse.json({ ports: [] })),
      tasksRead([]),
      workspaceAction(
        'suspend',
        problem(409, 'Workspace has an open delivery. Wait for it to finish.')
      )
    );
    const user = userEvent.setup();
    renderWorkspace('/workspaces/w1');
    await heading();
    expect(reads).toBe(1);
    await user.click(button('Suspend workspace'));
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Workspace has an open delivery. Wait for it to finish.'
    );
    await waitFor(() => expect(button('Suspend workspace')).toBeEnabled());
    expect(reads).toBe(2);
  });

  it("keeps a failed action's message when the lifecycle section unmounts", async () => {
    let reloading = false;
    server.use(
      workspaceRead(async () => {
        if (reloading) await delay(400);
        return HttpResponse.json(workspaceFixture());
      }),
      portsRead(() => HttpResponse.json({ ports: [] })),
      tasksRead([]),
      workspaceAction('suspend', async () => {
        await delay(150);
        return HttpResponse.json({ detail: 'Workspace has an open delivery.' }, { status: 409 });
      })
    );
    const user = userEvent.setup();
    const { queryClient } = renderWorkspace('/workspaces/w1');
    await heading();
    reloading = true;
    await user.click(button('Suspend workspace'));
    // Reset the workspace while the action is in flight. The page goes back to loading, the
    // lifecycle section unmounts, and the 409 arrives into that loading page.
    void queryClient.resetQueries({ queryKey: keys.workspace('w1') });
    expect(await screen.findByRole('alert')).toHaveTextContent('Workspace has an open delivery.');
    expect(screen.getByText('Loading workspace…')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Refresh status' })).not.toBeInTheDocument();
    await waitFor(() => expect(button('Suspend workspace')).toBeEnabled());
    expect(screen.getByRole('alert')).toHaveTextContent('Workspace has an open delivery.');
  });

  it('shows an inline error with a working retry when a refresh fails after the data loaded', async () => {
    let broken = false;
    server.use(
      workspaceRead(() =>
        broken
          ? HttpResponse.json({ detail: 'boom' }, { status: 500 })
          : HttpResponse.json(workspaceFixture())
      ),
      portsRead(() => HttpResponse.json({ ports: [] })),
      tasksRead([]),
      workspaceAction('refresh', () => {
        broken = true;
        return HttpResponse.json(workspaceFixture());
      })
    );
    const user = userEvent.setup();
    renderWorkspace('/workspaces/w1');
    await heading();
    await user.click(button('Refresh status'));
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent("Couldn't refresh this workspace");
    expect(alert).toHaveTextContent('Mainloop returned a server error.');
    // The data already on screen stays up beside the error.
    expect(button('Suspend workspace')).toBeInTheDocument();
    broken = false;
    await user.click(within(alert).getByRole('button', { name: 'Try again' }));
    await waitFor(() => expect(screen.queryByRole('alert')).not.toBeInTheDocument());
  });

  it('shows the state the backend reports when an action comes back unconfirmed', async () => {
    server.use(
      ...serveWorkspace(),
      workspaceAction('refresh', () =>
        HttpResponse.json(workspaceFixture({ observed_state: 'unknown' }))
      )
    );
    const user = userEvent.setup();
    renderWorkspace('/workspaces/w1');
    await heading();
    await user.click(button('Refresh status'));
    expect(await screen.findByRole('alert')).toHaveTextContent(UNCONFIRMED);
  });

  it('says so when the state itself is unconfirmed', async () => {
    server.use(...serveWorkspace(workspaceFixture({ observed_state: 'unknown' })));
    renderWorkspace('/workspaces/w1');
    await heading();
    expect(screen.getByText(UNCONFIRMED)).toBeInTheDocument();
  });

  it('shows "Workspace not found" for a 404, with no retry', async () => {
    server.use(
      workspaceRead(problem(404, 'Workspace not found')),
      portsRead(() => HttpResponse.json({ ports: [] })),
      tasksRead([])
    );
    renderWorkspace('/workspaces/w1');
    expect(
      await screen.findByRole('heading', { level: 1, name: 'Workspace not found' })
    ).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Try again' })).not.toBeInTheDocument();
  });

  it('shows a load error, not a half-filled page, for a response it cannot read', async () => {
    server.use(
      workspaceRead(() => HttpResponse.json({ ...workspaceFixture(), observed_state: 'exploded' })),
      portsRead(() => HttpResponse.json({ ports: [] })),
      tasksRead([])
    );
    renderWorkspace('/workspaces/w1');
    expect(
      await screen.findByRole('heading', { level: 1, name: 'Workspace could not be loaded' })
    ).toBeInTheDocument();
    expect(
      screen.getByText('Mainloop sent a workspace this page cannot read.')
    ).toBeInTheDocument();
  });

  it('skips a malformed task row and still links the workspace to its task', async () => {
    const good = view(
      { id: 'task-1', title: 'Add retry button' },
      { attempts: [attempt({ id: 'attempt-1', task_id: 'task-1', workspace_id: 'w1' })] }
    );
    server.use(
      workspaceRead(() => HttpResponse.json(workspaceFixture())),
      portsRead(() => HttpResponse.json({ ports: [] })),
      tasksRead([{ ...view({ id: 'task-bad' }), attempts: null }, good])
    );
    renderWorkspace('/workspaces/w1');
    await heading();
    const taskLink = await screen.findByRole('link', { name: 'Add retry button' });
    expect(taskLink).toHaveAttribute('href', '/tasks/task-1');
    expect(screen.getByTestId('workspace-task-link')).toBeInTheDocument();
  });

  it('lists readable manifest ports and says how many it skipped', async () => {
    server.use(
      ...serveWorkspace(
        workspaceFixture({
          manifest: {
            ...workspaceFixture().manifest,
            dev: {
              ports: [{ name: 'web', number: 3000 }, { name: 'broken' } as never],
              idle_timeout_minutes: 30
            }
          }
        })
      )
    );
    renderWorkspace('/workspaces/w1');
    await heading();
    expect(fact('Preview ports')).toHaveTextContent('web: 3000');
    expect(
      screen.getByText('One declared port could not be read and is not listed.')
    ).toBeInTheDocument();
  });

  it('reads previews from the server, uses each URL as given, and drops a javascript: row', async () => {
    server.use(
      ...serveWorkspace(workspaceFixture(), {
        ports: [
          { port: 3000, name: 'web', url: 'https://web.example.test/' },
          { port: 4000, name: 'evil', url: 'javascript:alert(1)' },
          { port: 5000, name: 'local', url: 'http://localhost:5000/' }
        ]
      })
    );
    renderWorkspace('/workspaces/w1');
    const web = await screen.findByRole('link', { name: 'Open preview web, port 3000' });
    expect(web).toHaveAttribute('href', 'https://web.example.test/');
    expect(web).toHaveAttribute('target', '_blank');
    expect(web).toHaveAttribute('rel', 'noopener noreferrer');
    expect(screen.getByRole('link', { name: 'Open preview local, port 5000' })).toHaveAttribute(
      'href',
      'http://localhost:5000/'
    );
    expect(screen.getAllByRole('link', { name: /^Open preview/ })).toHaveLength(2);
    expect(
      screen.getByText('One preview could not be read and is not listed.')
    ).toBeInTheDocument();
  });

  it('says when no preview ports are declared', async () => {
    server.use(...serveWorkspace());
    renderWorkspace('/workspaces/w1');
    expect(await screen.findByText('No preview ports are declared.')).toBeInTheDocument();
  });

  it('shows a preview load error with a working retry', async () => {
    let broken = true;
    server.use(
      workspaceRead(() => HttpResponse.json(workspaceFixture())),
      portsRead(() =>
        broken
          ? HttpResponse.json({ detail: 'boom' }, { status: 500 })
          : HttpResponse.json({ ports: [] })
      ),
      tasksRead([])
    );
    const user = userEvent.setup();
    renderWorkspace('/workspaces/w1');
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('Preview ports could not be loaded.');
    expect(alert).toHaveTextContent('Mainloop returned a server error.');
    broken = false;
    await user.click(within(alert).getByRole('button', { name: 'Try again' }));
    expect(await screen.findByText('No preview ports are declared.')).toBeInTheDocument();
  });

  it('keeps the page up and shows the reason when delete is refused with a 409', async () => {
    let reads = 0;
    server.use(
      workspaceRead(() => {
        reads += 1;
        return HttpResponse.json(workspaceFixture());
      }),
      portsRead(() => HttpResponse.json({ ports: [] })),
      tasksRead([]),
      workspaceDelete(problem(409, 'Workspace has an open delivery. Wait for it to finish.'))
    );
    const user = userEvent.setup();
    renderWorkspace('/workspaces/w1');
    await heading();
    await confirmDelete(user);
    expect(await screen.findByTestId('workspace-action-notice')).toHaveTextContent(
      'Workspace has an open delivery. Wait for it to finish.'
    );
    await waitFor(() => expect(button('Delete workspace')).toBeEnabled());
    expect(reads).toBe(2);
    expect(button('Suspend workspace')).toBeEnabled();
  });

  it('shows the server message for a 500 on delete, then re-reads the workspace', async () => {
    let reads = 0;
    server.use(
      workspaceRead(() => {
        reads += 1;
        return HttpResponse.json(workspaceFixture());
      }),
      portsRead(() => HttpResponse.json({ ports: [] })),
      tasksRead([]),
      workspaceDelete(problem(500, 'Could not remove the checkout.'))
    );
    const user = userEvent.setup();
    renderWorkspace('/workspaces/w1');
    await heading();
    await confirmDelete(user);
    expect(await screen.findByTestId('workspace-action-notice')).toHaveTextContent(
      'Could not remove the checkout.'
    );
    await waitFor(() => expect(button('Delete workspace')).toBeEnabled());
    expect(reads).toBe(2);
  });

  it('says the backend could not be reached when a delete request fails on the network', async () => {
    server.use(
      ...serveWorkspace(),
      workspaceDelete(() => HttpResponse.error())
    );
    const user = userEvent.setup();
    renderWorkspace('/workspaces/w1');
    await heading();
    await confirmDelete(user);
    expect(await screen.findByTestId('workspace-action-notice')).toHaveTextContent(
      "Couldn't reach Mainloop. Try again when it's back."
    );
    await waitFor(() => expect(button('Delete workspace')).toBeEnabled());
  });

  it('says the workspace is gone when a lost delete reply is followed by a 404 re-read', async () => {
    let deleted = false;
    server.use(
      workspaceRead(() =>
        deleted
          ? HttpResponse.json({ detail: 'Workspace not found' }, { status: 404 })
          : HttpResponse.json(workspaceFixture())
      ),
      portsRead(() => HttpResponse.json({ ports: [] })),
      tasksRead([]),
      workspaceDelete(() => {
        // The server removed the workspace, but its reply never reached the page.
        deleted = true;
        return HttpResponse.error();
      })
    );
    const user = userEvent.setup();
    renderWorkspace('/workspaces/w1');
    await heading();
    await confirmDelete(user);
    expect(
      await screen.findByRole('heading', { level: 1, name: 'Workspace not found' })
    ).toBeInTheDocument();
    expect(screen.getByTestId('workspace-action-notice')).toHaveTextContent(
      "Couldn't reach Mainloop. Try again when it's back."
    );
    expect(screen.queryByRole('button', { name: 'Try again' })).not.toBeInTheDocument();
  });

  it('drops the cached workspace and its ports once a delete succeeds', async () => {
    let deleted = false;
    const gone = () => HttpResponse.json({ detail: 'Workspace not found' }, { status: 404 });
    server.use(
      workspaceRead(() => (deleted ? gone() : HttpResponse.json(workspaceFixture()))),
      portsRead(() => (deleted ? gone() : HttpResponse.json({ ports: [] }))),
      tasksRead([]),
      workspaceDelete(() => {
        deleted = true;
        return new HttpResponse(null, { status: 204 });
      })
    );
    const user = userEvent.setup();
    const { queryClient } = renderWorkspace('/workspaces/w1');
    await heading();
    await confirmDelete(user);
    expect(await screen.findByText('Sessions list')).toBeInTheDocument();
    expect(queryClient.getQueryData(keys.workspace('w1'))).toBeUndefined();
    expect(queryClient.getQueryData(keys.workspacePorts('w1'))).toBeUndefined();
  });

  it('settles a suspending workspace when a live update says it is suspended', async () => {
    server.use(...serveWorkspace(workspaceFixture({ observed_state: 'suspending' })));
    const { queryClient } = renderWorkspace('/workspaces/w1');
    await heading();
    expect(button('Resume workspace')).toBeDisabled();
    expect(button('Suspend workspace')).toBeDisabled();

    // The same path the app's event stream takes: the applier writes the upsert into the cache.
    createSseApplier(queryClient)({
      event: 'workspace:updated',
      data: JSON.stringify({ workspace: workspaceFixture({ observed_state: 'suspended' }) })
    });

    await waitFor(() => expect(button('Resume workspace')).toBeEnabled());
    expect(button('Suspend workspace')).toBeDisabled();
    expect(screen.getByText('suspended', { selector: 'span' })).toBeInTheDocument();
  });

  it('settles a resuming workspace when a live update says it is running', async () => {
    server.use(...serveWorkspace(workspaceFixture({ observed_state: 'resuming' })));
    const { queryClient } = renderWorkspace('/workspaces/w1');
    await heading();
    expect(button('Resume workspace')).toBeDisabled();
    expect(button('Suspend workspace')).toBeDisabled();

    createSseApplier(queryClient)({
      event: 'workspace:updated',
      data: JSON.stringify({ workspace: workspaceFixture({ observed_state: 'running' }) })
    });

    await waitFor(() => expect(button('Suspend workspace')).toBeEnabled());
    expect(button('Resume workspace')).toBeDisabled();
  });

  it('keeps the last good workspace on screen when a live update cannot be read', async () => {
    server.use(...serveWorkspace(workspaceFixture({ observed_state: 'running' })));
    const user = userEvent.setup();
    const { queryClient } = renderWorkspace('/workspaces/w1');
    await heading();

    createSseApplier(queryClient)({
      event: 'workspace:updated',
      data: JSON.stringify({
        workspace: { ...workspaceFixture(), observed_state: 'exploded' }
      })
    });

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent("Couldn't refresh this workspace");
    expect(alert).toHaveTextContent(UNREADABLE);
    expect(screen.getByTestId('workspace-lifecycle-badge')).toHaveTextContent(/WS RUNNING/i);
    expect(button('Suspend workspace')).toBeEnabled();
    expect(
      screen.queryByRole('heading', { level: 1, name: 'Workspace could not be loaded' })
    ).not.toBeInTheDocument();

    await user.click(within(alert).getByRole('button', { name: 'Try again' }));
    await waitFor(() => expect(screen.queryByRole('alert')).not.toBeInTheDocument());
  });

  it('does not write an unreadable action result, and keeps the page up', async () => {
    server.use(
      ...serveWorkspace(workspaceFixture({ observed_state: 'running' })),
      workspaceAction('suspend', () =>
        HttpResponse.json({ ...workspaceFixture(), observed_state: 'exploded' })
      )
    );
    const user = userEvent.setup();
    renderWorkspace('/workspaces/w1');
    await heading();
    await user.click(button('Suspend workspace'));
    expect(await screen.findByTestId('workspace-action-notice')).toHaveTextContent(
      UNREADABLE_ACTION
    );
    expect(screen.getByTestId('workspace-lifecycle-badge')).toHaveTextContent(/WS RUNNING/i);
    expect(
      screen.queryByRole('heading', { level: 1, name: 'Workspace could not be loaded' })
    ).not.toBeInTheDocument();
  });

  it('shows the workspace reason alongside an action warning', async () => {
    server.use(
      ...serveWorkspace(workspaceFixture({ observed_state: 'failed', detail: 'Runtime exited.' })),
      workspaceAction('refresh', problem(409, 'Refresh is not available yet.'))
    );
    const user = userEvent.setup();
    renderWorkspace('/workspaces/w1');
    await heading();
    await user.click(button('Refresh status'));
    expect(await screen.findByTestId('workspace-action-notice')).toHaveTextContent(
      'Refresh is not available yet.'
    );
    expect(screen.getByText('Runtime exited.')).toBeInTheDocument();
  });
});

/** Opens the delete dialog and confirms it. */
async function confirmDelete(user: ReturnType<typeof userEvent.setup>) {
  await user.click(button('Delete workspace'));
  const dialog = await screen.findByRole('alertdialog');
  await user.click(within(dialog).getByRole('button', { name: 'Delete workspace' }));
}
