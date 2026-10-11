import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { delay, HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { connection, keys } from '@/api';
import { server } from '@/test/msw/server';
import {
  commit,
  detail,
  project,
  pullRequest,
  REPO_URL,
  workspace,
  type ProjectFixture
} from './test/fixtures';
import {
  createWorkspaceWrite,
  detailRead,
  serveProjectPage,
  workspacesReadWith
} from './test/handlers';
import { renderProjects } from './test/render';

// The page's reads would otherwise queue behind the app's read limiter. See fastReads.ts.
vi.mock('@/api/middleware', async (importOriginal) => {
  const { withFastReads } = await import('@/features/sessions/test/fastReads');
  return withFastReads(await importOriginal());
});

afterEach(async () => {
  await connection.check();
});

const PAGE = '/projects/project-a';

function heading(name: string) {
  return screen.findByRole('heading', { level: 1, name });
}

describe('project page', () => {
  it('a failed project detail read renders an error and retry instead of loading forever', async () => {
    let reads = 0;
    let releaseRetry!: () => void;
    const retryGate = new Promise<void>((resolve) => {
      releaseRetry = resolve;
    });
    server.use(
      detailRead(async () => {
        reads += 1;
        // The first read fails at once. The retry is held so the loading line can be seen.
        if (reads === 2) await retryGate;
        return HttpResponse.json({ detail: 'Failed to get project detail' }, { status: 500 });
      }),
      ...serveProjectPage()
    );
    const user = userEvent.setup();
    renderProjects(PAGE);

    expect(await screen.findByText('Loading project...')).toBeInTheDocument();
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent("Couldn't load the project");
    expect(alert).toHaveTextContent('Failed to get project detail');

    await user.click(screen.getByRole('button', { name: 'Retry loading project' }));
    expect(await screen.findByText('Loading project...')).toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();

    releaseRetry();
    expect(
      await screen.findByRole('button', { name: 'Retry loading project' })
    ).toBeInTheDocument();
    expect(screen.queryByText('Loading project...')).not.toBeInTheDocument();
    expect(reads).toBe(2);
  });

  it('shows the project, its pull requests, commits, and the tasks slot', async () => {
    server.use(...serveProjectPage());
    renderProjects(PAGE);

    expect(await heading('acme/widgets')).toBeInTheDocument();
    const github = screen.getByRole('link', { name: 'View on GitHub →' });
    expect(github).toHaveAttribute('href', REPO_URL);
    expect(github).toHaveAttribute('target', '_blank');
    expect(github).toHaveAttribute('rel', 'noopener noreferrer');
    expect(screen.getByText('1 open PR')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: '#42' })).toHaveAttribute(
      'href',
      'https://github.com/acme/widgets/pull/42'
    );
    // Only the first line of a commit message is listed, and the short sha links to the commit.
    expect(screen.getByText('Fix the widget lookup')).toBeInTheDocument();
    expect(screen.queryByText(/Long body that is not shown/)).not.toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'abcdef1' })).toBeInTheDocument();
    expect(screen.getByTestId('project-tasks')).toHaveTextContent('No tasks for this project yet.');
    expect(screen.getByTestId('merge-policy')).toBeInTheDocument();
    expect(screen.getByTestId('project-environment')).toBeInTheDocument();
  });

  it('drops a pull request or commit that breaks the contract and renders the rest', async () => {
    const brokenPr = { ...pullRequest({ title: 'Broken PR' }), number: 'not-a-number' };
    const brokenCommit = { ...commit({ message: 'Broken commit' }), sha: 123 };
    server.use(
      ...serveProjectPage({
        detail: detail({
          project: project(),
          open_prs: [
            pullRequest({ number: 7, title: 'Keep me' }),
            brokenPr as unknown as ReturnType<typeof pullRequest>
          ],
          recent_commits: [
            commit({ message: 'Keep this commit' }),
            brokenCommit as unknown as ReturnType<typeof commit>
          ]
        })
      })
    );
    renderProjects(PAGE);

    expect(await heading('acme/widgets')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: '#7' })).toBeInTheDocument();
    expect(screen.getByText('Keep me')).toBeInTheDocument();
    expect(screen.getByText('Keep this commit')).toBeInTheDocument();
    expect(screen.queryByText('Broken PR')).not.toBeInTheDocument();
    expect(screen.queryByText('Broken commit')).not.toBeInTheDocument();
  });

  it('shows a pull request with a non-https link as text, not as a link', async () => {
    server.use(
      ...serveProjectPage({
        detail: detail({
          project: project(),
          open_prs: [pullRequest({ number: 9, title: 'Odd link', url: 'javascript:alert(1)' })]
        })
      })
    );
    renderProjects(PAGE);

    expect(await heading('acme/widgets')).toBeInTheDocument();
    expect(screen.getByText('Odd link')).toBeInTheDocument();
    expect(screen.getByText('#9')).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: '#9' })).not.toBeInTheDocument();
  });

  it('a project that breaks the contract fails the page with a message, not a blank', async () => {
    const broken = { ...project(), merge_policy: 'mystery' } as unknown as ProjectFixture;
    server.use(...serveProjectPage({ detail: detail({ project: broken }) }));
    renderProjects(PAGE);

    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Mainloop sent a project that does not match the contract.'
    );
    expect(screen.queryByRole('heading', { level: 1 })).not.toBeInTheDocument();
  });

  it('a failed refresh keeps the loaded project with an inline error and a retry', async () => {
    let failing = false;
    server.use(
      detailRead(() =>
        failing
          ? HttpResponse.json({ detail: 'Project read failed' }, { status: 500 })
          : HttpResponse.json(detail())
      ),
      ...serveProjectPage()
    );
    const user = userEvent.setup();
    const { queryClient } = renderProjects(PAGE);
    await heading('acme/widgets');

    failing = true;
    await queryClient.invalidateQueries({ queryKey: keys.projectDetail('project-a') });
    const block = await screen.findByTestId('project-refresh-error');
    expect(block).toHaveTextContent("Couldn't refresh the project");
    expect(block).toHaveTextContent('Project read failed Showing the last loaded project.');
    expect(screen.getByRole('heading', { level: 1, name: 'acme/widgets' })).toBeInTheDocument();

    failing = false;
    await user.click(within(block).getByRole('button', { name: 'Retry loading project' }));
    expect(screen.queryByTestId('project-refresh-error')).not.toBeInTheDocument();
  });

  it('lists only branch workspaces of this repository, with their agent and state', async () => {
    server.use(
      ...serveProjectPage({
        workspaces: [
          workspace({ workspace_id: 'ws-1' }),
          workspace({
            workspace_id: 'ws-2',
            manifest: {
              ...workspace().manifest,
              repo_url: 'https://github.com/acme/other',
              branch: 'elsewhere'
            }
          })
        ]
      })
    );
    renderProjects(PAGE);

    await heading('acme/widgets');
    const link = await screen.findByRole('link', { name: /feature\/widgets/ });
    expect(link).toHaveAttribute('href', '/workspaces/ws-1');
    expect(within(link).getByTestId('workspace-agent-kind')).toHaveTextContent('claude');
    expect(screen.queryByText('elsewhere')).not.toBeInTheDocument();
  });

  it('a failed workspace read shows a retry for the workspaces, not the whole page', async () => {
    let calls = 0;
    server.use(
      workspacesReadWith(() => {
        calls += 1;
        return calls === 1
          ? HttpResponse.json({ detail: 'Workspaces are unavailable' }, { status: 500 })
          : HttpResponse.json([]);
      }),
      ...serveProjectPage({ workspaces: [] })
    );
    const user = userEvent.setup();
    renderProjects(PAGE);

    expect(await heading('acme/widgets')).toBeInTheDocument();
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent("Couldn't load branch workspaces");
    await user.click(screen.getByRole('button', { name: 'Retry loading workspaces' }));
    expect(await screen.findByText('No branch workspaces yet.')).toBeInTheDocument();
  });

  it('starts a workspace on the chosen branch with the chosen agent, then opens it', async () => {
    const bodies: Record<string, unknown>[] = [];
    server.use(
      createWorkspaceWrite(async ({ request }) => {
        bodies.push((await request.json()) as Record<string, unknown>);
        return HttpResponse.json({ workspace_id: 'ws-9' }, { status: 201 });
      }),
      ...serveProjectPage()
    );
    const user = userEvent.setup();
    renderProjects(PAGE);
    const form = await screen.findByTestId('project-workspace-form');

    const branch = within(form).getByRole('textbox', { name: 'Workspace branch' });
    expect(branch).toHaveValue('main');
    await user.clear(branch);
    await user.type(branch, 'feature/new');
    await user.click(screen.getByTestId('project-workspace-kind-codex'));
    await user.click(within(form).getByRole('button', { name: 'New workspace' }));

    expect(await screen.findByText('Workspace page')).toBeInTheDocument();
    expect(bodies).toEqual([
      {
        project_id: 'project-a',
        branch: 'feature/new',
        ref: '',
        depth: 0,
        dev: { ports: [], idle_timeout_minutes: 30 },
        agent_kind: 'codex'
      }
    ]);
  });

  it('an empty branch starts on the default branch', async () => {
    const bodies: Record<string, unknown>[] = [];
    server.use(
      createWorkspaceWrite(async ({ request }) => {
        bodies.push((await request.json()) as Record<string, unknown>);
        return HttpResponse.json({ workspace_id: 'ws-8' }, { status: 201 });
      }),
      ...serveProjectPage()
    );
    const user = userEvent.setup();
    renderProjects(PAGE);
    const form = await screen.findByTestId('project-workspace-form');

    await user.clear(within(form).getByRole('textbox', { name: 'Workspace branch' }));
    await user.click(within(form).getByRole('button', { name: 'New workspace' }));

    expect(await screen.findByText('Workspace page')).toBeInTheDocument();
    expect(bodies[0]).toMatchObject({
      project_id: 'project-a',
      branch: 'main',
      agent_kind: 'claude'
    });
  });

  it('a failed branch create keeps the backend message and blocks a second submit', async () => {
    let posts = 0;
    server.use(
      createWorkspaceWrite(async () => {
        posts += 1;
        await delay(200);
        return HttpResponse.json({ detail: 'Branch already has a workspace' }, { status: 409 });
      }),
      ...serveProjectPage()
    );
    const user = userEvent.setup();
    renderProjects(PAGE);
    const form = await screen.findByTestId('project-workspace-form');

    await user.click(within(form).getByRole('button', { name: 'New workspace' }));
    const pending = await within(form).findByRole('button', { name: 'Creating…' });
    expect(pending).toBeDisabled();
    await user.click(pending);

    expect(await screen.findByRole('alert')).toHaveTextContent('Branch already has a workspace');
    expect(posts).toBe(1);
    expect(within(form).getByRole('button', { name: 'New workspace' })).toBeEnabled();
  });

  it('is disabled while the backend is unreachable', async () => {
    connection.reportFailure();
    server.use(...serveProjectPage());
    renderProjects(PAGE);

    const form = await screen.findByTestId('project-workspace-form');
    expect(within(form).getByRole('button', { name: 'New workspace' })).toBeDisabled();
  });
});
