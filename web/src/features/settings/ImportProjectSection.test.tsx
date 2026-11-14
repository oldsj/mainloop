import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  Outlet,
  RouterProvider
} from '@tanstack/react-router';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { connection } from '@/api';
import { project } from '@/features/projects/test/fixtures';
import { problem, projectImport } from '@/features/projects/test/handlers';
import { server } from '@/test/msw/server';
import { ImportProjectSection } from './ImportProjectSection';

vi.mock('@/api/middleware', async (importOriginal) => {
  const { withFastReads } = await import('@/features/sessions/test/fastReads');
  return withFastReads(await importOriginal());
});

afterEach(async () => {
  await connection.check();
});

function renderSection() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } }
  });
  const root = createRootRoute({ component: Outlet });
  const router = createRouter({
    routeTree: root.addChildren([
      createRoute({
        getParentRoute: () => root,
        path: '/settings',
        component: ImportProjectSection
      }),
      createRoute({
        getParentRoute: () => root,
        path: '/projects/$id',
        component: () => <p>Project page</p>
      })
    ]),
    history: createMemoryHistory({ initialEntries: ['/settings'] })
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  );
}

describe('import project section', () => {
  it('is reachable at #import-project', async () => {
    renderSection();
    const heading = await screen.findByRole('heading', { name: 'Import a project' });
    expect(heading.closest('section')).toHaveAttribute('id', 'import-project');
    expect(screen.getByRole('button', { name: 'Import' })).toBeDisabled();
  });

  it('imports the typed repository and links to the new project', async () => {
    let body: unknown;
    server.use(
      projectImport(async ({ request }) => {
        body = await request.json();
        return HttpResponse.json(
          project({ id: 'project-oc', full_name: 'acme/cozy', name: 'cozy' }),
          { status: 201 }
        );
      })
    );
    const user = userEvent.setup();
    renderSection();
    await user.type(await screen.findByRole('textbox', { name: 'Repository' }), ' acme/cozy ');
    await user.click(screen.getByRole('button', { name: 'Import' }));
    expect(await screen.findByText('Imported acme/cozy.')).toBeInTheDocument();
    expect(body).toEqual({ repo: 'acme/cozy' });
    expect(screen.getByRole('textbox', { name: 'Repository' })).toHaveValue('');
    await user.click(screen.getByRole('link', { name: 'Open acme/cozy' }));
    expect(await screen.findByText('Project page')).toBeInTheDocument();
  });

  it("shows the server's refusal and keeps what was typed", async () => {
    server.use(projectImport(problem(400, 'Invalid GitHub repository')));
    const user = userEvent.setup();
    renderSection();
    await user.type(await screen.findByRole('textbox', { name: 'Repository' }), 'not a repo');
    await user.click(screen.getByRole('button', { name: 'Import' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Invalid GitHub repository');
    expect(screen.getByRole('textbox', { name: 'Repository' })).toHaveValue('not a repo');
  });

  it('says so when the imported project breaks the contract', async () => {
    server.use(projectImport(() => HttpResponse.json({ id: '' }, { status: 201 })));
    const user = userEvent.setup();
    renderSection();
    await user.type(await screen.findByRole('textbox', { name: 'Repository' }), 'acme/cozy');
    await user.click(screen.getByRole('button', { name: 'Import' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Mainloop imported the repository but sent a project that does not match the contract.'
    );
  });
});
