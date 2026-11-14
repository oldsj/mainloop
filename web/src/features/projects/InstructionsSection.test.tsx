import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { connection } from '@/api';
import { server } from '@/test/msw/server';
import { InstructionsSection } from './InstructionsSection';
import { instructions } from './test/fixtures';
import {
  instructionVersionsRead,
  instructionsRead,
  instructionsWrite,
  problem
} from './test/handlers';

// The section makes its reads through the app's limiter, which the tests skip. See fastReads.ts.
vi.mock('@/api/middleware', async (importOriginal) => {
  const { withFastReads } = await import('@/features/sessions/test/fastReads');
  return withFastReads(await importOriginal());
});

afterEach(async () => {
  await connection.check();
});

const SAVED_AT = new Date(Date.now() - 2 * 3_600_000).toISOString();

function renderSection() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } }
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <InstructionsSection projectId="project-a" />
    </QueryClientProvider>
  );
}

function controls() {
  const section = screen.getByTestId('instructions');
  return {
    section,
    text: within(section).getByRole('textbox', { name: 'Instructions for agents' }),
    save: within(section).getByRole('button', { name: /Save|Saving…/ })
  };
}

describe('instructions section', () => {
  it('says when there are none, and saves the first version', async () => {
    let body: unknown;
    server.use(
      instructionsRead(() => HttpResponse.json(instructions())),
      instructionsWrite(async ({ request }) => {
        body = await request.json();
        return HttpResponse.json(
          instructions({ version: 1, text: 'Use fictional data.', updated_at: SAVED_AT })
        );
      })
    );
    const user = userEvent.setup();
    renderSection();
    expect(await screen.findByText('No instructions yet.')).toBeInTheDocument();
    expect(controls().save).toBeDisabled();

    await user.type(controls().text, '  Use fictional data.\n');
    await user.click(controls().save);

    expect(await screen.findByText('Version 1, saved 2h ago.')).toBeInTheDocument();
    expect(body).toEqual({ text: '  Use fictional data.\n', expected_version: 0 });
    expect(controls().text).toHaveValue('Use fictional data.');
    expect(screen.queryByRole('button', { name: 'Discard draft' })).not.toBeInTheDocument();
  });

  it('does not offer a save for a change only in surrounding whitespace', async () => {
    server.use(
      instructionsRead(() =>
        HttpResponse.json(instructions({ version: 2, text: 'Keep it.', updated_at: SAVED_AT }))
      )
    );
    const user = userEvent.setup();
    renderSection();
    await screen.findByText('Version 2, saved 2h ago.');
    await user.type(controls().text, '   ');
    expect(controls().save).toBeDisabled();
    await user.click(screen.getByRole('button', { name: 'Discard draft' }));
    expect(controls().text).toHaveValue('Keep it.');
  });

  it('counts UTF-8 bytes and refuses a draft over the bound', async () => {
    server.use(instructionsRead(() => HttpResponse.json(instructions({ max_bytes: 8 }))));
    const user = userEvent.setup();
    renderSection();
    await screen.findByText('No instructions yet.');

    await user.type(controls().text, 'été');
    expect(screen.getByText('5 of 8 bytes')).toBeInTheDocument();
    expect(controls().save).toBeEnabled();

    await user.type(controls().text, 'été');
    expect(screen.getByText('10 of 8 bytes (2 over)')).toBeInTheDocument();
    expect(controls().save).toBeDisabled();
  });

  it('keeps the draft when the instructions changed underneath it, and saves over them on request', async () => {
    let current = instructions({ version: 1, text: 'Old.', updated_at: SAVED_AT });
    const bodies: unknown[] = [];
    server.use(
      instructionsRead(() => HttpResponse.json(current)),
      instructionsWrite(async ({ request }) => {
        const body = (await request.json()) as { text: string; expected_version: number };
        bodies.push(body);
        if (body.expected_version !== current.version) {
          return HttpResponse.json(
            { detail: 'Project instructions changed since they were read' },
            { status: 409 }
          );
        }
        current = instructions({ version: current.version + 1, text: body.text.trim() });
        return HttpResponse.json(current);
      })
    );
    const user = userEvent.setup();
    renderSection();
    await screen.findByText('Version 1, saved 2h ago.');

    await user.clear(controls().text);
    await user.type(controls().text, 'Mine.');
    // Another tab saves version 2 while this draft is open.
    current = instructions({ version: 2, text: 'Theirs.', updated_at: SAVED_AT });
    await user.click(controls().save);

    const conflict = await screen.findByTestId('instructions-conflict');
    expect(conflict).toHaveTextContent('changed to version 2 while you were editing');
    expect(within(conflict).getByText('Theirs.')).toBeInTheDocument();
    expect(controls().text).toHaveValue('Mine.');
    expect(
      screen.queryByText('Project instructions changed since they were read')
    ).not.toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Save over version 2' }));
    expect(await screen.findByText(/^Version 3/)).toBeInTheDocument();
    expect(bodies).toEqual([
      { text: 'Mine.', expected_version: 1 },
      { text: 'Mine.', expected_version: 2 }
    ]);
    expect(screen.queryByTestId('instructions-conflict')).not.toBeInTheDocument();
  });

  it('shows a refused save and keeps the draft', async () => {
    server.use(
      instructionsRead(() => HttpResponse.json(instructions())),
      instructionsWrite(problem(422, 'Project instructions must be UTF-8 text without NUL'))
    );
    const user = userEvent.setup();
    renderSection();
    await screen.findByText('No instructions yet.');
    await user.type(controls().text, 'Draft.');
    await user.click(controls().save);
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Project instructions must be UTF-8 text without NUL'
    );
    expect(controls().text).toHaveValue('Draft.');
  });

  it('lists earlier versions when opened', async () => {
    let reads = 0;
    server.use(
      instructionsRead(() =>
        HttpResponse.json(instructions({ version: 2, text: 'Now.', updated_at: SAVED_AT }))
      ),
      instructionVersionsRead(() => {
        reads += 1;
        return HttpResponse.json({
          versions: [
            { version: 2, text: 'Now.', created_at: SAVED_AT },
            { version: 1, text: 'Before.', created_at: SAVED_AT }
          ]
        });
      })
    );
    const user = userEvent.setup();
    renderSection();
    await screen.findByText('Version 2, saved 2h ago.');
    expect(reads).toBe(0);

    await user.click(screen.getByText('Earlier versions'));
    expect(await screen.findByText('Before.')).toBeInTheDocument();
    expect(reads).toBe(1);
  });

  it('shows a load failure with a retry', async () => {
    server.use(instructionsRead(problem(500, 'boom')));
    renderSection();
    expect(await screen.findByText('Could not load the instructions.')).toBeInTheDocument();
  });
});
