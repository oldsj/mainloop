import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { connection, keys } from '@/api';
import { session, type SessionFixture } from '@/features/sessions/test/fixtures';
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
  detailRead,
  providersReadWith,
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

  it('opens with tasks, open pull requests and sessions, and puts settings last', async () => {
    server.use(...serveProjectPage());
    renderProjects(PAGE);
    await heading('acme/widgets');

    const sections = screen
      .getAllByRole('heading', { level: 2 })
      .map((item) => item.textContent?.replace(/\[[-+]\]$/, '').trim());
    expect(sections).toEqual([
      'Tasks',
      'Open Pull Requests',
      'Sessions',
      'Branch workspaces',
      'Recent Commits',
      'Settings'
    ]);
  });

  it('folds merge policy and environment under Settings, collapsed until opened', async () => {
    server.use(...serveProjectPage());
    const user = userEvent.setup();
    renderProjects(PAGE);
    await heading('acme/widgets');

    const toggle = screen.getByRole('button', { name: 'Settings' });
    const body = document.getElementById('project-settings-body');
    expect(toggle).toHaveAttribute('aria-expanded', 'false');
    expect(toggle).toHaveAttribute('aria-controls', 'project-settings-body');
    expect(body).not.toBeVisible();
    // Hidden content is out of the accessibility tree, so the sections are found by test id.
    expect(screen.getByTestId('merge-policy')).not.toBeVisible();

    await user.click(toggle);
    expect(toggle).toHaveAttribute('aria-expanded', 'true');
    expect(body).toBeVisible();
    expect(screen.getByTestId('merge-policy')).toBeVisible();
    expect(screen.getByTestId('project-environment')).toBeVisible();

    await user.click(toggle);
    expect(toggle).toHaveAttribute('aria-expanded', 'false');
    expect(body).not.toBeVisible();
  });

  it('opens Settings from the keyboard and keeps focus on the toggle', async () => {
    server.use(...serveProjectPage());
    const user = userEvent.setup();
    renderProjects(PAGE);
    await heading('acme/widgets');

    const toggle = screen.getByRole('button', { name: 'Settings' });
    toggle.focus();
    await user.keyboard('{Enter}');
    expect(toggle).toHaveAttribute('aria-expanded', 'true');
    expect(toggle).toHaveFocus();
    expect(screen.getByTestId('merge-policy')).toBeVisible();
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

  it('links to orchestration health for this project', async () => {
    server.use(...serveProjectPage());
    renderProjects(PAGE);

    expect(await heading('acme/widgets')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Orchestration health' })).toHaveAttribute(
      'href',
      '/health?project_id=project-a'
    );
  });

  it('names the page in the document title', async () => {
    server.use(...serveProjectPage());
    renderProjects(PAGE);

    expect(await heading('acme/widgets')).toBeInTheDocument();
    await waitFor(() => expect(document.title).toBe('acme/widgets · Mainloop'));
  });

  it('shows when a branch workspace was last active', async () => {
    server.use(
      ...serveProjectPage({
        workspaces: [
          workspace({
            workspace_id: 'ws-1',
            last_activity_at: new Date(Date.now() - 5 * 60_000).toISOString()
          })
        ]
      })
    );
    renderProjects(PAGE);

    await heading('acme/widgets');
    const link = await screen.findByRole('link', { name: /feature\/widgets/ });
    expect(within(link).getByText(/^Active /)).toBeInTheDocument();
  });

  it('drops a session row that breaks the contract and keeps the rest', async () => {
    const broken = {
      ...session({ id: 's-bad', title: 'Broken session', project_id: 'project-a' }),
      repo_url: 42
    } as unknown as SessionFixture;
    server.use(
      ...serveProjectPage({
        detail: detail({
          project: project(),
          sessions: [
            session({ id: 's-good', title: 'Good session', project_id: 'project-a' }),
            broken
          ]
        })
      })
    );
    renderProjects(PAGE);

    expect(await screen.findByText('Good session')).toBeInTheDocument();
    expect(screen.queryByText('Broken session')).not.toBeInTheDocument();
  });

  it('a provider profile that breaks the contract does not hide the sessions', async () => {
    server.use(
      providersReadWith(() =>
        HttpResponse.json([{ id: 'claude-main', native_provider: 'claude', aliases: null }])
      ),
      ...serveProjectPage({
        detail: detail({
          project: project(),
          sessions: [session({ id: 's-good', title: 'Still listed', project_id: 'project-a' })]
        })
      })
    );
    renderProjects(PAGE);

    expect(await screen.findByText('Still listed')).toBeInTheDocument();
  });
});

describe('project page creates nothing', () => {
  it('has no new-task or new-workspace control: work starts from the main chat', async () => {
    server.use(...serveProjectPage());
    renderProjects(PAGE);
    await heading('acme/widgets');
    expect(screen.queryByRole('button', { name: 'New task' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'New workspace' })).not.toBeInTheDocument();
    expect(screen.queryByTestId('project-workspace-form')).not.toBeInTheDocument();
    expect(screen.queryByRole('link', { name: /new agent/i })).not.toBeInTheDocument();
  });
});
