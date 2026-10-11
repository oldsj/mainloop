import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { delay, HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { connection, keys } from '@/api';
import { server } from '@/test/msw/server';
import { project, type ProjectFixture } from './test/fixtures';
import {
  createWorkspaceWrite,
  projectsRead,
  projectsReadWith,
  workspacesRead,
  workspacesReadWith
} from './test/handlers';
import { renderProjects } from './test/render';

// The panel's reads would otherwise queue behind the app's read limiter. See fastReads.ts.
vi.mock('@/api/middleware', async (importOriginal) => {
  const { withFastReads } = await import('@/features/sessions/test/fastReads');
  return withFastReads(await importOriginal());
});

afterEach(async () => {
  await connection.check();
});

const REPO_BODY = {
  ref: '',
  depth: 0,
  dev: { ports: [], idle_timeout_minutes: 30 }
};

/** The router renders asynchronously, so the toggle is awaited rather than read straight away. */
async function openForm(user: ReturnType<typeof userEvent.setup>) {
  await user.click(await screen.findByRole('button', { name: 'New workspace' }));
  return screen.getByTestId('new-workspace-form');
}

describe('projects list', () => {
  it('lists each project with its name, linking to its page', async () => {
    server.use(
      projectsRead([
        project(),
        project({ id: 'project-b', full_name: 'acme/gadgets', avatar_url: null, description: null })
      ])
    );
    renderProjects('/');

    const list = await screen.findByTestId('projects-list');
    expect(within(list).getByRole('link', { name: /acme\/widgets/ })).toHaveAttribute(
      'href',
      '/projects/project-a'
    );
    expect(within(list).getByRole('link', { name: /acme\/gadgets/ })).toHaveAttribute(
      'href',
      '/projects/project-b'
    );
  });

  it('shows the empty state when there are no projects', async () => {
    server.use(projectsRead([]));
    renderProjects('/');
    expect(await screen.findByText('No projects yet')).toBeInTheDocument();
    expect(screen.queryByTestId('projects-list')).not.toBeInTheDocument();
  });

  it('drops a row that breaks the contract and renders the rest', async () => {
    const broken = {
      ...project({ id: 'broken', full_name: 'acme/broken' }),
      merge_policy: 'mystery'
    };
    server.use(projectsRead([project(), broken as unknown as ProjectFixture]));
    renderProjects('/');

    const list = await screen.findByTestId('projects-list');
    expect(within(list).getAllByRole('link')).toHaveLength(1);
    expect(within(list).getByRole('link', { name: /acme\/widgets/ })).toBeInTheDocument();
    expect(screen.queryByText('acme/broken')).not.toBeInTheDocument();
  });

  it('shows a retryable error when the first load fails', async () => {
    let calls = 0;
    server.use(
      projectsReadWith(() => {
        calls += 1;
        return calls === 1
          ? HttpResponse.json({ detail: 'Projects are unavailable' }, { status: 500 })
          : HttpResponse.json([project()]);
      })
    );
    const user = userEvent.setup();
    renderProjects('/');

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent("Couldn't load projects");
    expect(alert).toHaveTextContent('Projects are unavailable');
    await user.click(screen.getByRole('button', { name: 'Retry loading projects' }));
    expect(await screen.findByTestId('projects-list')).toBeInTheDocument();
  });

  it('a failed refresh keeps the cached list with an inline error and a retry', async () => {
    let failing = false;
    server.use(
      projectsReadWith(() =>
        failing
          ? HttpResponse.json({ detail: 'Projects read failed' }, { status: 500 })
          : HttpResponse.json([project()])
      )
    );
    const user = userEvent.setup();
    const { queryClient } = renderProjects('/');
    await screen.findByTestId('projects-list');

    failing = true;
    await queryClient.invalidateQueries({ queryKey: keys.projects() });
    const block = await screen.findByTestId('projects-refresh-error');
    expect(block).toHaveTextContent("Couldn't refresh projects");
    expect(block).toHaveTextContent('Projects read failed Showing the last list.');
    expect(screen.getByTestId('projects-list')).toBeInTheDocument();

    failing = false;
    await user.click(within(block).getByRole('button', { name: 'Retry loading projects' }));
    expect(screen.queryByTestId('projects-refresh-error')).not.toBeInTheDocument();
  });
});

describe('new workspace from a repository', () => {
  it('refuses an empty repository and sends nothing', async () => {
    let posts = 0;
    server.use(
      projectsRead([project()]),
      createWorkspaceWrite(() => {
        posts += 1;
        return HttpResponse.json({ detail: 'unexpected' }, { status: 500 });
      })
    );
    const user = userEvent.setup();
    renderProjects('/');
    const form = await openForm(user);

    await user.click(within(form).getByRole('button', { name: 'Create workspace' }));
    expect(await within(form).findByRole('alert')).toHaveTextContent('Enter a repository');
    expect(posts).toBe(0);
  });

  it('creates from an owner/name repository and opens the new workspace', async () => {
    const bodies: Record<string, unknown>[] = [];
    server.use(
      projectsRead([project()]),
      createWorkspaceWrite(async ({ request }) => {
        bodies.push((await request.json()) as Record<string, unknown>);
        return HttpResponse.json({ workspace_id: 'ws-1' }, { status: 201 });
      })
    );
    const user = userEvent.setup();
    renderProjects('/');
    const form = await openForm(user);

    await user.type(within(form).getByRole('textbox', { name: 'Repository' }), '  acme/widgets  ');
    await user.click(within(form).getByRole('button', { name: 'Create workspace' }));

    expect(await screen.findByText('Workspace page')).toBeInTheDocument();
    expect(bodies).toEqual([
      { ...REPO_BODY, repo: 'acme/widgets', branch: '', agent_kind: 'claude' }
    ]);
  });

  it('sends a GitHub URL, the chosen branch and the chosen agent', async () => {
    const bodies: Record<string, unknown>[] = [];
    server.use(
      projectsRead([project()]),
      createWorkspaceWrite(async ({ request }) => {
        bodies.push((await request.json()) as Record<string, unknown>);
        return HttpResponse.json({ workspace_id: 'ws-2' }, { status: 201 });
      })
    );
    const user = userEvent.setup();
    renderProjects('/');
    const form = await openForm(user);

    await user.type(
      within(form).getByRole('textbox', { name: 'Repository' }),
      'https://github.com/acme/widgets'
    );
    await user.type(
      within(form).getByRole('textbox', { name: 'Branch (optional)' }),
      'feature/cache'
    );
    await user.click(screen.getByTestId('new-workspace-kind-codex'));
    await user.click(within(form).getByRole('button', { name: 'Create workspace' }));

    expect(await screen.findByText('Workspace page')).toBeInTheDocument();
    expect(bodies).toEqual([
      {
        ...REPO_BODY,
        repo: 'https://github.com/acme/widgets',
        branch: 'feature/cache',
        agent_kind: 'codex'
      }
    ]);
  });

  it('refetches the project list after a create, whether it succeeds or fails', async () => {
    let listReads = 0;
    server.use(
      projectsReadWith(() => {
        listReads += 1;
        return HttpResponse.json([project()]);
      }),
      createWorkspaceWrite(() =>
        HttpResponse.json({ detail: 'Workspace already exists' }, { status: 409 })
      )
    );
    const user = userEvent.setup();
    renderProjects('/');
    const form = await openForm(user);
    await user.type(within(form).getByRole('textbox', { name: 'Repository' }), 'acme/widgets');
    const readsBefore = listReads;

    await user.click(within(form).getByRole('button', { name: 'Create workspace' }));
    expect(await screen.findByText('Workspace already exists')).toBeInTheDocument();
    expect(listReads).toBeGreaterThan(readsBefore);
  });

  it('a failed create keeps the backend message after the form closes', async () => {
    server.use(
      projectsRead([project()]),
      createWorkspaceWrite(async () => {
        await delay(200);
        return HttpResponse.json({ detail: 'Repository is not accessible' }, { status: 403 });
      })
    );
    const user = userEvent.setup();
    renderProjects('/');
    const form = await openForm(user);
    await user.type(within(form).getByRole('textbox', { name: 'Repository' }), 'acme/private');
    await user.click(within(form).getByRole('button', { name: 'Create workspace' }));
    expect(await screen.findByRole('button', { name: 'Creating…' })).toBeDisabled();

    // Closing the form while the request is in flight must not drop its outcome.
    await user.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Repository is not accessible');
    expect(screen.queryByTestId('new-workspace-form')).not.toBeInTheDocument();
  });

  it('does not submit twice while a create is in flight', async () => {
    let posts = 0;
    server.use(
      projectsRead([project()]),
      createWorkspaceWrite(async () => {
        posts += 1;
        await delay(200);
        return HttpResponse.json({ workspace_id: 'ws-3' }, { status: 201 });
      })
    );
    const user = userEvent.setup();
    renderProjects('/');
    const form = await openForm(user);
    await user.type(within(form).getByRole('textbox', { name: 'Repository' }), 'acme/widgets');
    await user.click(within(form).getByRole('button', { name: 'Create workspace' }));

    const pending = await screen.findByRole('button', { name: 'Creating…' });
    expect(pending).toBeDisabled();
    await user.click(pending);

    expect(await screen.findByText('Workspace page')).toBeInTheDocument();
    expect(posts).toBe(1);
  });

  it.each([
    ['no reply', () => HttpResponse.error()],
    ['a 500', () => HttpResponse.json({ detail: 'Mainloop is restarting' }, { status: 500 })]
  ])(
    'a create with %s may still have made a workspace, so the form stays locked until the list is checked',
    async (_case, reply) => {
      let posts = 0;
      server.use(
        projectsRead([project()]),
        workspacesRead([]),
        createWorkspaceWrite(() => {
          posts += 1;
          return reply();
        })
      );
      const user = userEvent.setup();
      renderProjects('/');
      const form = await openForm(user);
      await user.type(within(form).getByRole('textbox', { name: 'Repository' }), 'acme/widgets');
      await user.click(within(form).getByRole('button', { name: 'Create workspace' }));

      const check = await screen.findByTestId('create-check');
      expect(check).toHaveTextContent('The workspace may have been created.');
      // The raw failure is not shown while the owner checks; the check replaces it.
      expect(screen.queryByText('Mainloop is restarting')).not.toBeInTheDocument();
      await within(check).findByText('No recent workspaces are listed.');

      const submit = within(form).getByRole('button', { name: 'Create workspace' });
      expect(submit).toBeDisabled();
      await user.click(submit);
      expect(posts).toBe(1);

      const acknowledge = within(check).getByRole('button', { name: "I've checked the list" });
      await waitFor(() => expect(acknowledge).toBeEnabled());
      await user.click(acknowledge);
      expect(screen.queryByTestId('create-check')).not.toBeInTheDocument();
      // The reply that never came marked the backend offline. The health probe clears it, as it would in the app.
      await act(async () => {
        await connection.check();
      });
      expect(within(form).getByRole('button', { name: 'Create workspace' })).toBeEnabled();
    }
  );

  it('a failed re-read of the workspace list keeps the check locked, with a retry', async () => {
    // Every list read after the create fails, so the check has nothing it can confirm.
    let posted = false;
    server.use(
      projectsRead([project()]),
      workspacesReadWith(() =>
        posted
          ? HttpResponse.json({ detail: 'Workspaces are unavailable' }, { status: 500 })
          : HttpResponse.json([])
      ),
      createWorkspaceWrite(() => {
        posted = true;
        return HttpResponse.error();
      })
    );
    const user = userEvent.setup();
    renderProjects('/');
    const form = await openForm(user);
    await user.type(within(form).getByRole('textbox', { name: 'Repository' }), 'acme/widgets');
    await user.click(within(form).getByRole('button', { name: 'Create workspace' }));

    const check = await screen.findByTestId('create-check');
    expect(await within(check).findByText("Couldn't read the workspace list.")).toBeInTheDocument();
    // A list that failed to re-read cannot confirm anything, so there is no way on yet.
    expect(within(check).getByRole('button', { name: "I've checked the list" })).toBeDisabled();
    expect(within(check).getByRole('button', { name: 'Retry loading workspaces' })).toBeEnabled();
  });

  it('is disabled while the backend is unreachable', async () => {
    connection.reportFailure();
    server.use(projectsRead([project()]));
    const user = userEvent.setup();
    renderProjects('/');
    const form = await openForm(user);

    expect(within(form).getByRole('button', { name: 'Create workspace' })).toBeDisabled();
  });
});
