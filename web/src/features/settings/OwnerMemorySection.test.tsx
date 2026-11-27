import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { connection, keys } from '@/api';
import { API_URL } from '@/api/config';
import { project } from '@/features/projects/test/fixtures';
import { dotfilesHandlers } from '@/test/msw/handlers';
import { server } from '@/test/msw/server';
import {
  daysAgo,
  ownerEntry,
  ownerMemoryBackend,
  refusal,
  renderOwnerMemory
} from './test/ownerMemory';

// The section's reads would otherwise queue behind the app's read limiter. See fastReads.ts.
vi.mock('@/api/middleware', async (importOriginal) => {
  const { withFastReads } = await import('@/features/sessions/test/fastReads');
  return withFastReads(await importOriginal());
});

afterEach(async () => {
  await connection.check();
});

const MAIN = ownerEntry();
const PROPOSAL = ownerEntry({
  id: 'om-2',
  text: 'Prefer small pull requests.',
  state: 'proposed',
  author: 'task',
  source: {
    session_id: 'sup-1',
    task_id: 'task-7',
    attempt_id: 'att-1',
    delivery_id: null,
    owner_entry_id: null
  },
  replaces_id: 'om-1',
  created_at: daysAgo(1),
  last_confirmed_at: null,
  decided_at: null,
  due_at: null
});
// Confirmed 20 days ago with a 14-day window: due, still loaded.
const DUE = ownerEntry({
  id: 'om-3',
  text: 'Write commit messages in the imperative.',
  created_at: daysAgo(30),
  last_confirmed_at: daysAgo(20)
});
const LAPSED = ownerEntry({
  id: 'om-4',
  text: 'Use tabs.',
  state: 'lapsed',
  created_at: daysAgo(40),
  last_confirmed_at: daysAgo(25),
  lapsed_at: '2026-11-20T09:00:00Z'
});

function serve(...args: Parameters<typeof ownerMemoryBackend>) {
  const backend = ownerMemoryBackend(...args);
  server.use(...backend.handlers);
  return backend;
}

describe('owner memory section', () => {
  it('says when there is no memory', async () => {
    serve();
    renderOwnerMemory();
    const section = await screen.findByTestId('owner-memory');
    expect(await within(section).findByText('No memory yet.')).toBeInTheDocument();
    expect(within(section).getByText('0 of 3,072 bytes used')).toBeInTheDocument();
    expect(within(section).queryByTestId('owner-memory-proposals')).not.toBeInTheDocument();
    expect(within(section).queryByTestId('owner-memory-review')).not.toBeInTheDocument();
    expect(within(section).getByLabelText('Ask me if still true after')).toHaveValue(14);
  });

  it('lists proposals, entries to review and active entries, each with its age and source', async () => {
    serve({ entries: [MAIN, DUE, LAPSED, PROPOSAL] });
    renderOwnerMemory();

    const proposals = await screen.findByTestId('owner-memory-proposals');
    expect(
      within(proposals).getByRole('heading', { name: 'Waiting for you (1)' })
    ).toBeInTheDocument();
    const proposal = within(proposals).getByTestId('owner-memory-entry-om-2');
    expect(within(proposal).getByText(`Replaces: ${MAIN.text}`)).toBeInTheDocument();
    expect(within(proposal).getByText('Proposed by a task, 1d ago')).toBeInTheDocument();
    expect(within(proposal).getByRole('link', { name: 'the task' })).toHaveAttribute(
      'href',
      '/tasks/task-7'
    );
    expect(
      within(proposal).getByRole('button', { name: `Accept: ${PROPOSAL.text}` })
    ).toBeEnabled();
    expect(
      within(proposal).getByRole('button', { name: `Reject: ${PROPOSAL.text}` })
    ).toBeEnabled();

    // Oldest confirmation first: the lapsed entry (25 days) before the due one (20 days).
    const review = screen.getByTestId('owner-memory-review');
    expect(within(review).getByRole('heading', { name: 'Still true? (2)' })).toBeInTheDocument();
    const reviewed = within(review).getAllByRole('listitem');
    expect(reviewed.map((item) => item.dataset.testid)).toEqual([
      'owner-memory-entry-om-4',
      'owner-memory-entry-om-3'
    ]);
    expect(within(reviewed[0]!).getByText('Not loaded since 2026-11-20')).toBeInTheDocument();
    expect(within(reviewed[0]!).getByRole('button', { name: 'Keep: Use tabs.' })).toBeEnabled();
    expect(within(reviewed[0]!).getByRole('button', { name: 'Forget: Use tabs.' })).toBeEnabled();
    expect(within(reviewed[1]!).queryByText(/Not loaded since/)).not.toBeInTheDocument();

    // Every loaded entry, the due one included; the lapsed one is not loaded.
    const active = screen.getByTestId('owner-memory-active');
    const entry = within(active).getByTestId('owner-memory-entry-om-1');
    expect(within(entry).getByText('By main, 3d ago')).toBeInTheDocument();
    expect(within(entry).getByRole('link', { name: 'from this turn' })).toHaveAttribute(
      'href',
      '/'
    );
    expect(within(entry).getByText('· confirmed 3 days ago')).toBeInTheDocument();
    expect(within(entry).getByRole('button', { name: `Forget: ${MAIN.text}` })).toBeEnabled();
    expect(
      within(entry).getByRole('button', { name: `Move to project: ${MAIN.text}` })
    ).toBeEnabled();
    expect(
      within(entry).getByRole('button', { name: `Move to dotfiles: ${MAIN.text}` })
    ).toBeEnabled();
    expect(within(active).getByText('· confirmed 20 days ago')).toBeInTheDocument();
    expect(within(active).queryByTestId('owner-memory-entry-om-4')).not.toBeInTheDocument();
    const used = MAIN.text.length + DUE.text.length;
    expect(within(active).getByText(`${used} of 3,072 bytes used`)).toBeInTheDocument();
  });

  it.each([
    ['Accept', PROPOSAL, 'accept'],
    ['Reject', PROPOSAL, 'reject'],
    ['Keep', LAPSED, 'keep'],
    ['Forget', MAIN, 'forget']
  ] as const)(
    '%s sends its decision and shows the memory as it now is',
    async (label, entry, decision) => {
      const backend = serve({ entries: [MAIN, LAPSED, PROPOSAL] });
      const user = userEvent.setup();
      const { queryClient } = renderOwnerMemory();
      // The Needs you snapshot the shell would hold, with the owner memory item in it.
      queryClient.setQueryData(keys.attention(), { items: [], count: 1, sessions: [] });
      const buttons = await screen.findAllByRole('button', { name: `${label}: ${entry.text}` });
      await user.click(buttons[0]!);

      await waitFor(() =>
        expect(backend.decisions).toEqual([{ entryId: entry.id, body: { decision } }])
      );
      await waitFor(() =>
        expect(queryClient.getQueryState(keys.attention())?.isInvalidated).toBe(true)
      );
      const active = screen.getByTestId('owner-memory-active');
      if (decision === 'accept') {
        expect(await within(active).findByTestId('owner-memory-entry-om-2')).toBeInTheDocument();
        expect(screen.queryByTestId('owner-memory-proposals')).not.toBeInTheDocument();
      }
      if (decision === 'reject') {
        await waitFor(() =>
          expect(screen.queryByTestId('owner-memory-proposals')).not.toBeInTheDocument()
        );
        expect(within(active).queryByTestId('owner-memory-entry-om-2')).not.toBeInTheDocument();
      }
      if (decision === 'keep') {
        expect(await within(active).findByTestId('owner-memory-entry-om-4')).toBeInTheDocument();
        expect(screen.queryByTestId('owner-memory-review')).not.toBeInTheDocument();
        expect(within(active).getByText('· confirmed today')).toBeInTheDocument();
      }
      if (decision === 'forget') {
        await waitFor(() =>
          expect(within(active).queryByTestId('owner-memory-entry-om-1')).not.toBeInTheDocument()
        );
      }
    }
  );

  it('shows a refused decision and the memory as it now is', async () => {
    const backend = serve({ entries: [MAIN, PROPOSAL] }, () =>
      refusal(409, 'Owner memory would pass 3072 bytes; forget an entry first')
    );
    const user = userEvent.setup();
    renderOwnerMemory();
    await user.click(await screen.findByRole('button', { name: `Accept: ${PROPOSAL.text}` }));
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Owner memory would pass 3072 bytes; forget an entry first'
    );
    expect(screen.getByTestId('owner-memory-entry-om-2')).toBeInTheDocument();
    expect(backend.decisions).toHaveLength(1);
  });

  it('drops an entry that breaks the contract and keeps the rest', async () => {
    const broken = { ...ownerEntry({ id: 'om-x', text: 'Broken' }), author: 'someone' };
    serve({ entries: [MAIN, broken as never] });
    renderOwnerMemory();
    expect(await screen.findByTestId('owner-memory-entry-om-1')).toBeInTheDocument();
    expect(screen.queryByText('Broken')).not.toBeInTheDocument();
  });

  it('saves the review window as it changes, and every due date moves with it', async () => {
    const backend = serve({ entries: [MAIN] });
    const user = userEvent.setup();
    renderOwnerMemory();
    const field = await screen.findByLabelText('Ask me if still true after');
    expect(screen.queryByTestId('owner-memory-review')).not.toBeInTheDocument();
    await user.clear(field);
    await user.type(field, '2');
    // MAIN was confirmed 3 days ago, so a 2-day window makes it due.
    const review = await screen.findByTestId('owner-memory-review', {}, { timeout: 3000 });
    expect(within(review).getByTestId('owner-memory-entry-om-1')).toBeInTheDocument();
    expect(backend.settings).toEqual([2]);
    expect(field).toHaveValue(2);
  });

  it('does not send a review window out of range', async () => {
    const backend = serve({ entries: [MAIN] });
    const user = userEvent.setup();
    renderOwnerMemory();
    const field = await screen.findByLabelText('Ask me if still true after');
    await user.clear(field);
    await user.type(field, '400');
    expect(
      await screen.findByText('Enter a whole number of days from 1 to 365.')
    ).toBeInTheDocument();
    await new Promise((resolve) => setTimeout(resolve, 600));
    expect(backend.settings).toEqual([]);
  });

  it('moves an entry to a project', async () => {
    const backend = serve({ entries: [MAIN] });
    server.use(
      http.get(`${API_URL}/projects`, () =>
        HttpResponse.json([project(), project({ id: 'project-b', full_name: 'oldsj/other' })])
      )
    );
    const user = userEvent.setup();
    renderOwnerMemory();
    await user.click(await screen.findByRole('button', { name: `Move to project: ${MAIN.text}` }));
    const panel = screen.getByTestId('owner-memory-move-project-om-1');
    const move = within(panel).getByRole('button', { name: 'Move' });
    expect(move).toBeDisabled();
    await user.selectOptions(
      await within(panel).findByLabelText('Move to the memory of'),
      'project-b'
    );
    await user.click(move);
    await waitFor(() =>
      expect(backend.decisions).toEqual([
        { entryId: 'om-1', body: { decision: 'move_to_project', project_id: 'project-b' } }
      ])
    );
    await waitFor(() =>
      expect(screen.queryByTestId('owner-memory-entry-om-1')).not.toBeInTheDocument()
    );
  });

  it('shows a refused move to a project and keeps the entry', async () => {
    serve({ entries: [MAIN] }, (body) =>
      body.decision === 'move_to_project'
        ? refusal(
            409,
            "The project's memory would pass 4096 bytes; revoke one of its entries first"
          )
        : undefined
    );
    server.use(http.get(`${API_URL}/projects`, () => HttpResponse.json([project()])));
    const user = userEvent.setup();
    renderOwnerMemory();
    await user.click(await screen.findByRole('button', { name: `Move to project: ${MAIN.text}` }));
    await user.selectOptions(await screen.findByLabelText('Move to the memory of'), 'project-a');
    await user.click(screen.getByRole('button', { name: 'Move' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(
      "The project's memory would pass 4096 bytes; revoke one of its entries first"
    );
    expect(screen.getByTestId('owner-memory-entry-om-1')).toBeInTheDocument();
  });

  it('flags an entry already in the dotfiles and offers to forget it', async () => {
    const backend = serve({
      entries: [ownerEntry({ in_dotfiles_commit: '0123456789abcdef0123456789abcdef01234567' })]
    });
    const user = userEvent.setup();
    renderOwnerMemory();
    const flag = await screen.findByTestId('owner-memory-in-dotfiles');
    expect(flag).toHaveTextContent('Already in your dotfiles at 0123456; forget it here?');
    await user.click(screen.getByRole('button', { name: `Forget: ${MAIN.text}` }));
    await waitFor(() =>
      expect(backend.decisions).toEqual([{ entryId: 'om-1', body: { decision: 'forget' } }])
    );
  });

  it('moves an entry to the dotfiles: the text to copy, where it goes, then "I\'ve moved it"', async () => {
    const backend = serve({ entries: [MAIN] });
    server.use(
      ...dotfilesHandlers({
        owner: { repository: 'oldsj/dotfiles', ref: 'main', path: 'agents', version: 1 }
      })
    );
    const user = userEvent.setup();
    renderOwnerMemory();
    await user.click(await screen.findByRole('button', { name: `Move to dotfiles: ${MAIN.text}` }));
    const panel = screen.getByTestId('owner-memory-move-dotfiles-om-1');
    const text = await within(panel).findByRole('textbox');
    expect(text).toHaveValue(MAIN.text);
    expect(panel).toHaveTextContent('agents/AGENTS.md in oldsj/dotfiles (main)');
    await user.click(within(panel).getByRole('button', { name: "I've moved it" }));
    await waitFor(() =>
      expect(backend.decisions).toEqual([{ entryId: 'om-1', body: { decision: 'moved' } }])
    );
    await waitFor(() =>
      expect(screen.queryByTestId('owner-memory-entry-om-1')).not.toBeInTheDocument()
    );
  });

  it('links to the dotfiles setup when no dotfiles source is configured', async () => {
    const backend = serve({ entries: [MAIN] });
    const user = userEvent.setup();
    renderOwnerMemory();
    await user.click(await screen.findByRole('button', { name: `Move to dotfiles: ${MAIN.text}` }));
    const panel = screen.getByTestId('owner-memory-move-dotfiles-om-1');
    expect(
      await within(panel).findByRole('link', { name: 'Set up agent dotfiles' })
    ).toHaveAttribute('href', '/settings#dotfiles');
    expect(within(panel).queryByRole('button', { name: "I've moved it" })).not.toBeInTheDocument();
    expect(backend.decisions).toEqual([]);
  });
});
