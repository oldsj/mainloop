import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { connection } from '@/api';
import { server } from '@/test/msw/server';
import { commandFromDraft, WorkspaceServiceSection } from './WorkspaceServiceSection';
import { problem, workspaceServiceRead, workspaceServiceWrite } from './test/handlers';

// The section makes its reads through the app's limiter, which the tests skip. See fastReads.ts.
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
  return render(
    <QueryClientProvider client={queryClient}>
      <WorkspaceServiceSection projectId="project-a" />
    </QueryClientProvider>
  );
}

function controls() {
  const section = screen.getByTestId('workspace-service');
  return {
    command: within(section).getByRole('textbox', { name: 'Command, one argument per line' }),
    save: within(section).getByRole('button', { name: /Save|Saving…|Remove service/ })
  };
}

describe('commandFromDraft', () => {
  it('reads one argument per line, trimming ends and skipping blank lines', () => {
    expect(commandFromDraft('  scripts/dev/workspace.sh \n\n run\r\n')).toEqual({
      command: ['scripts/dev/workspace.sh', 'run'],
      error: null
    });
    expect(commandFromDraft('')).toEqual({ command: [], error: null });
  });

  it('names the bound a draft breaks', () => {
    expect(commandFromDraft(Array.from({ length: 62 }, () => 'a').join('\n')).error).toBe(
      'At most 61 arguments; this has 62.'
    );
    expect(commandFromDraft(`ok\n${'é'.repeat(2049)}`).error).toBe(
      'Argument 2 is over 4096 bytes.'
    );
  });
});

describe('workspace service section', () => {
  it('says when no service is declared', async () => {
    server.use(workspaceServiceRead(() => HttpResponse.json({ command: [] })));
    renderSection();
    expect(
      await screen.findByText('No workspace service. Workspaces start no server of their own.')
    ).toBeInTheDocument();
    expect(controls().save).toBeDisabled();
  });

  it('saves the command as an argv and shows what runs', async () => {
    let body: Record<string, unknown> | undefined;
    server.use(
      workspaceServiceRead(() => HttpResponse.json({ command: [] })),
      workspaceServiceWrite(async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json(body);
      })
    );
    const user = userEvent.setup();
    renderSection();
    await screen.findByText(/No workspace service/);
    await user.type(controls().command, 'scripts/dev/workspace.sh{Enter}run');
    await user.click(controls().save);
    expect(
      await screen.findByText('scripts/dev/workspace.sh run', { selector: 'code' })
    ).toBeInTheDocument();
    expect(body).toEqual({ command: ['scripts/dev/workspace.sh', 'run'] });
    expect(controls().save).toBeDisabled();
  });

  it('removes a declared service with an empty command', async () => {
    let body: Record<string, unknown> | undefined;
    server.use(
      workspaceServiceRead(() => HttpResponse.json({ command: ['npm', 'run', 'dev'] })),
      workspaceServiceWrite(async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json(body);
      })
    );
    const user = userEvent.setup();
    renderSection();
    expect(await screen.findByText('npm run dev', { selector: 'code' })).toBeInTheDocument();
    await user.clear(controls().command);
    await user.click(screen.getByRole('button', { name: 'Remove service' }));
    expect(await screen.findByText(/No workspace service/)).toBeInTheDocument();
    expect(body).toEqual({ command: [] });
  });

  it('keeps a draft over the bounds from being sent', async () => {
    let writes = 0;
    server.use(
      workspaceServiceRead(() => HttpResponse.json({ command: [] })),
      workspaceServiceWrite(() => {
        writes += 1;
        return HttpResponse.json({ command: [] });
      })
    );
    const user = userEvent.setup();
    renderSection();
    await screen.findByText(/No workspace service/);
    await user.click(controls().command);
    await user.paste(Array.from({ length: 62 }, (_, i) => `a${i}`).join('\n'));
    expect(screen.getByRole('alert')).toHaveTextContent('At most 61 arguments; this has 62.');
    expect(controls().save).toBeDisabled();
    expect(writes).toBe(0);
  });

  it('shows a refused save and keeps the draft', async () => {
    server.use(
      workspaceServiceRead(() => HttpResponse.json({ command: [] })),
      workspaceServiceWrite(problem(422, 'command is too long for the workspace'))
    );
    const user = userEvent.setup();
    renderSection();
    await screen.findByText(/No workspace service/);
    await user.type(controls().command, 'serve');
    await user.click(controls().save);
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'command is too long for the workspace'
    );
    expect(controls().command).toHaveValue('serve');
  });

  it('offers a retry when the read fails', async () => {
    server.use(workspaceServiceRead(problem(500, 'boom')));
    renderSection();
    expect(await screen.findByText("Couldn't load the workspace service.")).toBeInTheDocument();
    expect(
      screen.getByRole('button', { name: 'Retry loading workspace service' })
    ).toBeInTheDocument();
  });

  it('refuses a read that breaks the contract', async () => {
    server.use(workspaceServiceRead(() => HttpResponse.json({ command: 'npm run dev' })));
    renderSection();
    expect(
      await screen.findByText('Mainloop sent a workspace service that does not match the contract.')
    ).toBeInTheDocument();
    expect(screen.queryByRole('textbox')).not.toBeInTheDocument();
  });
});
