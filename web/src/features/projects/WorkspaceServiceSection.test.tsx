import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { connection } from '@/api';
import { server } from '@/test/msw/server';
import {
  commandFromDraft,
  portsFromDraft,
  WorkspaceServiceSection
} from './WorkspaceServiceSection';
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
    ports: within(section).getByRole('textbox', {
      name: 'Preview ports, one name=number per line'
    }),
    save: within(section).getByRole('button', { name: /Save|Saving…/ })
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

describe('portsFromDraft', () => {
  it('reads one name=number per line', () => {
    expect(portsFromDraft(' web=3000 \n\napi = 8080')).toEqual({
      ports: [
        { name: 'web', number: 3000 },
        { name: 'api', number: 8080 }
      ],
      error: null
    });
  });

  it('refuses what the server would', () => {
    expect(portsFromDraft('Web=3000').error).toMatch(/"Web=3000" is not a port/);
    expect(portsFromDraft('web=0').error).toMatch(/is not a port/);
    expect(portsFromDraft('web=65536').error).toMatch(/is not a port/);
    expect(portsFromDraft('3000').error).toMatch(/is not a port/);
    expect(portsFromDraft('web=3000\napi=3000').error).toBe(
      'Each port needs its own name and number.'
    );
    expect(portsFromDraft('web=3000\nweb=3001').error).toBe(
      'Each port needs its own name and number.'
    );
    expect(
      portsFromDraft(Array.from({ length: 17 }, (_, i) => `p${i}=${3000 + i}`).join('\n')).error
    ).toBe('At most 16 ports; this has 17.');
  });
});

describe('workspace service section', () => {
  it('says when no service is declared', async () => {
    server.use(workspaceServiceRead(() => HttpResponse.json({ command: [], ports: [] })));
    renderSection();
    expect(
      await screen.findByText('No workspace service. Workspaces start no server of their own.')
    ).toBeInTheDocument();
    expect(controls().save).toBeDisabled();
  });

  it('saves the command as an argv with its ports and shows what runs', async () => {
    let body: Record<string, unknown> | undefined;
    server.use(
      workspaceServiceRead(() => HttpResponse.json({ command: [], ports: [] })),
      workspaceServiceWrite(async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json(body);
      })
    );
    const user = userEvent.setup();
    renderSection();
    await screen.findByText(/No workspace service/);
    await user.type(controls().command, 'scripts/dev/workspace.sh{Enter}run');
    await user.type(controls().ports, 'web=3000');
    await user.click(controls().save);
    expect(
      await screen.findByText('scripts/dev/workspace.sh run', { selector: 'code' })
    ).toBeInTheDocument();
    expect(screen.getByText('Preview ports: web 3000')).toBeInTheDocument();
    expect(body).toEqual({
      command: ['scripts/dev/workspace.sh', 'run'],
      ports: [{ name: 'web', number: 3000 }]
    });
    expect(controls().save).toBeDisabled();
  });

  it('removes a declared service with an empty command and keeps its ports', async () => {
    let body: Record<string, unknown> | undefined;
    server.use(
      workspaceServiceRead(() =>
        HttpResponse.json({
          command: ['npm', 'run', 'dev'],
          ports: [{ name: 'web', number: 3000 }]
        })
      ),
      workspaceServiceWrite(async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json(body);
      })
    );
    const user = userEvent.setup();
    renderSection();
    expect(await screen.findByText('npm run dev', { selector: 'code' })).toBeInTheDocument();
    await user.clear(controls().command);
    await user.click(controls().save);
    expect(await screen.findByText(/No workspace service/)).toBeInTheDocument();
    expect(body).toEqual({ command: [], ports: [{ name: 'web', number: 3000 }] });
  });

  it('keeps a draft over the bounds from being sent', async () => {
    let writes = 0;
    server.use(
      workspaceServiceRead(() => HttpResponse.json({ command: [], ports: [] })),
      workspaceServiceWrite(() => {
        writes += 1;
        return HttpResponse.json({ command: [], ports: [] });
      })
    );
    const user = userEvent.setup();
    renderSection();
    await screen.findByText(/No workspace service/);
    await user.click(controls().command);
    await user.paste(Array.from({ length: 62 }, (_, i) => `a${i}`).join('\n'));
    expect(screen.getByRole('alert')).toHaveTextContent('At most 61 arguments; this has 62.');
    expect(controls().save).toBeDisabled();
    await user.clear(controls().command);
    await user.type(controls().ports, 'web=99999');
    expect(screen.getByRole('alert')).toHaveTextContent('"web=99999" is not a port.');
    expect(controls().save).toBeDisabled();
    expect(writes).toBe(0);
  });

  it('shows a refused save and keeps the draft', async () => {
    server.use(
      workspaceServiceRead(() => HttpResponse.json({ command: [], ports: [] })),
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
    server.use(workspaceServiceRead(() => HttpResponse.json({ command: ['npm'] })));
    renderSection();
    expect(
      await screen.findByText('Mainloop sent a workspace service that does not match the contract.')
    ).toBeInTheDocument();
    expect(screen.queryByRole('textbox')).not.toBeInTheDocument();
  });
});
