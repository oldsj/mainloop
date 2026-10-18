import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { act } from 'react';
import { HttpResponse, delay } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { connection, keys } from '@/api';
import { createSseApplier } from '@/api/invalidation';
import { createFakeHITL } from '@/features/hitl/handlers';
import { forgetAnswered } from '@/features/hitl/hitl-client';
import { server } from '@/test/msw/server';
import { hitlView } from '@/test/fixtures/hitl';
import {
  activityRow,
  attentionItem,
  attentionSnapshot,
  hitlAttentionItem,
  noticeRow,
  queueItem
} from './test/fixtures';
import {
  activityRead,
  attentionRead,
  dismissPost,
  problem,
  respondPost,
  tasksRead
} from './test/handlers';
import { renderInbox } from './test/render';

afterEach(async () => {
  await connection.check();
});

describe('Needs you', () => {
  it('shows the empty state when nothing needs the owner', async () => {
    server.use(attentionRead(() => HttpResponse.json(attentionSnapshot([]))));
    renderInbox();
    expect(await screen.findByText('Nothing needs you')).toBeInTheDocument();
  });

  it('shows the good rows and hides a malformed one', async () => {
    const good = attentionItem(queueItem({ id: 'good', title: 'Which branch?' }));
    const bad = attentionItem(
      queueItem({ id: 'bad', title: 'Broken row', options: 'main' as never })
    );
    server.use(attentionRead(() => HttpResponse.json(attentionSnapshot([good, bad]))));
    renderInbox();

    expect(await screen.findByText('Which branch?')).toBeInTheDocument();
    expect(screen.queryByText('Broken row')).toBeNull();
  });

  it('shows the checking state while the first read is pending', async () => {
    server.use(attentionRead(() => new Promise(() => {})));
    renderInbox();
    expect(await screen.findByRole('status')).toHaveTextContent('Checking what needs you…');
  });

  it('shows a question with its options and sends the chosen one', async () => {
    const user = userEvent.setup();
    const bodies: unknown[] = [];
    const item = queueItem({ options: ['main', 'release'] });
    server.use(
      attentionRead(() => HttpResponse.json(attentionSnapshot([attentionItem(item)]))),
      respondPost(async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json({ status: 'ok', message: 'Sent' });
      })
    );
    renderInbox();

    expect(await screen.findByText('Which branch?')).toBeInTheDocument();
    expect(document.getElementById('attention-q-1')).not.toBeNull();
    await user.click(screen.getByRole('button', { name: 'main' }));

    await waitFor(() => expect(bodies).toEqual([{ response: 'main' }]));
    expect(await screen.findByText('> main')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'release' })).toBeNull();
  });

  it('sends a free-text reply and shows it after the server accepts it', async () => {
    const user = userEvent.setup();
    const bodies: unknown[] = [];
    server.use(
      attentionRead(() => HttpResponse.json(attentionSnapshot([attentionItem(queueItem())]))),
      respondPost(async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json({ status: 'ok', message: 'Sent' });
      })
    );
    renderInbox();

    await user.type(await screen.findByPlaceholderText('Type your response...'), 'Use main');
    await user.click(screen.getByRole('button', { name: 'Send' }));

    await waitFor(() => expect(bodies).toEqual([{ response: 'Use main' }]));
    expect(await screen.findByText('> Use main')).toBeInTheDocument();
  });

  it('shows a plan review with its plan folded away until opened', async () => {
    const user = userEvent.setup();
    const plan = queueItem({
      id: 'p-1',
      item_type: 'plan_review',
      title: 'Review the plan',
      content: 'Step one: migrate the inbox.',
      context: { pr_url: 'https://github.com/owner/repo/pull/7' }
    });
    server.use(attentionRead(() => HttpResponse.json(attentionSnapshot([attentionItem(plan)]))));
    renderInbox();

    expect(await screen.findByText('Review the plan')).toBeInTheDocument();
    expect(screen.queryByText('Step one: migrate the inbox.')).toBeNull();
    await user.click(screen.getByRole('button', { name: 'View plan' }));
    expect(screen.getByText('Step one: migrate the inbox.')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'View PR' })).toHaveAttribute(
      'href',
      'https://github.com/owner/repo/pull/7'
    );
    expect(screen.getByPlaceholderText('Request changes...')).toBeInTheDocument();
  });

  it('renders a live HITL request as a card and reads it', async () => {
    const hitl = createFakeHITL({ 'req-1': hitlView('req-1') });
    server.use(
      attentionRead(() =>
        HttpResponse.json(attentionSnapshot([hitlAttentionItem('req-1', 'Approve the deploy.')]))
      ),
      tasksRead,
      ...hitl.handlers
    );
    renderInbox();

    expect(await screen.findByText('Approve the deploy.')).toBeInTheDocument();
    expect(document.getElementById('attention-req-1')).not.toBeNull();
    await waitFor(() => expect(hitl.reads).toContain('req-1'));
  });

  it('links a row without a queue item to its task', async () => {
    const row = attentionItem(queueItem(), {
      queue_item: null,
      task_id: 'task-9',
      title: 'Pick a deploy window'
    });
    server.use(attentionRead(() => HttpResponse.json(attentionSnapshot([row]))));
    renderInbox();

    expect(await screen.findByText('Pick a deploy window')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Review task and decide' })).toHaveAttribute(
      'href',
      '/tasks/task-9'
    );
  });

  it('links an unresolved runtime stop to task evidence', async () => {
    const row = attentionItem(queueItem(), {
      id: 'reconciliation:task-9',
      reason_code: 'runtime_stop_unresolved',
      queue_item: null,
      task_id: 'task-9',
      title: 'Recover stopped child'
    });
    server.use(attentionRead(() => HttpResponse.json(attentionSnapshot([row]))));
    renderInbox();
    expect(await screen.findByRole('link', { name: 'Review task evidence' })).toHaveAttribute(
      'href',
      '/tasks/task-9'
    );
  });

  it('links a stalled turn to its thread', async () => {
    const main = attentionItem(queueItem(), {
      id: 'stalled:msg-1',
      kind: 'owner_decision',
      reason_code: 'stalled',
      queue_item: null,
      task_id: null,
      href: '/',
      title: 'Main thread'
    });
    const other = attentionItem(queueItem(), {
      id: 'stalled:msg-2',
      kind: 'owner_decision',
      reason_code: 'stalled',
      queue_item: null,
      task_id: null,
      href: '/sessions/s-2',
      title: 'Fix the thing'
    });
    server.use(attentionRead(() => HttpResponse.json(attentionSnapshot([main, other]))));
    renderInbox();
    expect(await screen.findByRole('link', { name: 'Open the main thread' })).toHaveAttribute(
      'href',
      '/'
    );
    expect(screen.getByRole('link', { name: 'Open the session' })).toHaveAttribute(
      'href',
      '/sessions/s-2'
    );
  });

  it('shows an error with retry when the first read fails, then recovers', async () => {
    const user = userEvent.setup();
    let reads = 0;
    server.use(
      attentionRead(() => {
        reads++;
        return reads === 1
          ? HttpResponse.json({ detail: 'Backend unavailable' }, { status: 500 })
          : HttpResponse.json(attentionSnapshot([]));
      })
    );
    renderInbox();

    expect(await screen.findByText('Could not check what needs you.')).toBeInTheDocument();
    expect(screen.getByText('Backend unavailable')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Try again' }));
    expect(await screen.findByText('Nothing needs you')).toBeInTheDocument();
  });

  it('keeps the rows on screen when a refresh fails', async () => {
    let reads = 0;
    server.use(
      attentionRead(() => {
        reads++;
        return reads === 1
          ? HttpResponse.json(attentionSnapshot([attentionItem(queueItem())]))
          : HttpResponse.json({ detail: 'Backend unavailable' }, { status: 500 });
      })
    );
    const { queryClient } = renderInbox();

    expect(await screen.findByText('Which branch?')).toBeInTheDocument();
    await act(() => queryClient.invalidateQueries({ queryKey: keys.attention() }));
    expect(await screen.findByText('Could not check what needs you.')).toBeInTheDocument();
    expect(screen.getByText('Which branch?')).toBeInTheDocument();
  });

  it('refreshes when an inbox:updated event arrives', async () => {
    let reads = 0;
    server.use(
      attentionRead(() => {
        reads++;
        return HttpResponse.json(
          attentionSnapshot(reads === 1 ? [] : [attentionItem(queueItem())])
        );
      })
    );
    const { queryClient } = renderInbox();
    expect(await screen.findByText('Nothing needs you')).toBeInTheDocument();

    createSseApplier(queryClient)({ event: 'inbox:updated', data: '{}' });

    expect(await screen.findByText('Which branch?')).toBeInTheDocument();
  });

  it('brings the selected row to the top, again as its card grows, until the owner scrolls', async () => {
    // Each observer fires only while connected, as the real one does.
    const observers: { fire: () => void; live: boolean }[] = [];
    vi.stubGlobal(
      'ResizeObserver',
      class {
        entry: { fire: () => void; live: boolean };
        constructor(callback: ResizeObserverCallback) {
          this.entry = { fire: () => callback([], this as never), live: false };
          observers.push(this.entry);
        }
        observe() {
          this.entry.live = true;
        }
        disconnect() {
          this.entry.live = false;
        }
      }
    );
    const grow = () => observers.filter((o) => o.live).forEach((o) => o.fire());
    const scrolls: { key?: string; block?: ScrollLogicalPosition }[] = [];
    const original = Element.prototype.scrollIntoView;
    Element.prototype.scrollIntoView = function (this: HTMLElement, arg?: unknown) {
      scrolls.push({
        key: this.dataset.attentionKey,
        block: (arg as ScrollIntoViewOptions | undefined)?.block
      });
    };
    try {
      const rows = ['q-1', 'q-2'].map((id) => attentionItem(queueItem({ id, title: id })));
      // The refetch adds a row, so the list really changes under the selected one.
      let reads = 0;
      const later = attentionItem(queueItem({ id: 'q-3', title: 'q-3' }));
      server.use(
        attentionRead(() => HttpResponse.json(attentionSnapshot(reads++ ? [...rows, later] : rows)))
      );
      const { queryClient } = renderInbox('/', 'q-2');
      await waitFor(() => expect(scrolls).toEqual([{ key: 'q-2', block: 'start' }]));
      act(grow);
      expect(scrolls).toHaveLength(2);
      // A refetch re-renders the list but does not end the settling.
      await act(() => queryClient.refetchQueries({ queryKey: keys.attention() }));
      expect(await screen.findByText('q-3')).toBeInTheDocument();
      act(grow);
      expect(scrolls).toHaveLength(3);
      // Once the owner scrolls, a later growth leaves the page where they put it.
      act(() => window.dispatchEvent(new Event('wheel')));
      act(grow);
      expect(scrolls).toHaveLength(3);
    } finally {
      Element.prototype.scrollIntoView = original;
      vi.unstubAllGlobals();
    }
  });

  it('caches attention under the key SSE invalidation uses', async () => {
    server.use(attentionRead(() => HttpResponse.json(attentionSnapshot([]))));
    const { queryClient } = renderInbox();

    expect(await screen.findByText('Nothing needs you')).toBeInTheDocument();
    expect(queryClient.getQueryData(keys.attention())).toBeDefined();
  });
});

describe('dismissing an unavailable input notice from Activity', () => {
  it('offers Dismiss only on a row the server marks dismissible', async () => {
    const user = userEvent.setup();
    const pinned = noticeRow({
      id: 'queue:n-2',
      queue_item_id: 'n-2',
      dismissible: false,
      title: 'Pinned notice'
    });
    server.use(
      attentionRead(() => HttpResponse.json(attentionSnapshot([]))),
      activityRead(() => HttpResponse.json([activityRow(), noticeRow(), pinned]))
    );
    renderInbox();
    await screen.findByText('Nothing needs you');

    await user.click(screen.getByRole('button', { name: 'Activity' }));

    expect(await screen.findByText('Pinned notice')).toBeInTheDocument();
    expect(screen.getAllByRole('button', { name: 'Dismiss notice' })).toHaveLength(1);
    const pinnedRow = screen.getByText('Pinned notice').closest('li') as HTMLElement;
    expect(within(pinnedRow).queryByRole('button', { name: 'Dismiss notice' })).toBeNull();
  });

  it('shows the good rows and hides a malformed one', async () => {
    const user = userEvent.setup();
    server.use(
      attentionRead(() => HttpResponse.json(attentionSnapshot([]))),
      activityRead(() =>
        HttpResponse.json([activityRow({ title: 'Good row' }), { ...activityRow(), title: null }])
      )
    );
    renderInbox();
    await screen.findByText('Nothing needs you');

    await user.click(screen.getByRole('button', { name: 'Activity' }));

    expect(await screen.findByText('Good row')).toBeInTheDocument();
    expect(
      within(screen.getByRole('list', { name: 'Activity' })).getAllByRole('listitem')
    ).toHaveLength(1);
  });

  it('removes the notice at once and keeps it gone when the server accepts', async () => {
    const user = userEvent.setup();
    let open = true;
    const dismissed: string[] = [];
    server.use(
      attentionRead(() => HttpResponse.json(attentionSnapshot([]))),
      activityRead(() => HttpResponse.json(open ? [noticeRow()] : [])),
      dismissPost(({ params }) => {
        dismissed.push(String(params.item_id));
        open = false;
        return HttpResponse.json({ status: 'ok' });
      })
    );
    renderInbox();
    await screen.findByText('Nothing needs you');
    await user.click(screen.getByRole('button', { name: 'Activity' }));

    await user.click(await screen.findByRole('button', { name: 'Dismiss notice' }));

    await waitFor(() => expect(screen.queryByText('Input notice unavailable')).toBeNull());
    expect(await screen.findByText('No activity yet')).toBeInTheDocument();
    expect(dismissed).toEqual(['n-1']);
  });

  it('restores the notice and shows the server reason when the dismiss is refused', async () => {
    const user = userEvent.setup();
    server.use(
      attentionRead(() => HttpResponse.json(attentionSnapshot([]))),
      activityRead(() => HttpResponse.json([noticeRow()])),
      dismissPost(problem(500, 'Could not dismiss the unavailable input notice'))
    );
    renderInbox();
    await screen.findByText('Nothing needs you');
    await user.click(screen.getByRole('button', { name: 'Activity' }));

    await user.click(await screen.findByRole('button', { name: 'Dismiss notice' }));

    expect(
      await screen.findByText('Could not dismiss the unavailable input notice')
    ).toBeInTheDocument();
    expect(screen.getByText('Input notice unavailable')).toBeInTheDocument();
  });

  it('keeps a refusal on screen after the row has left, then re-reads', async () => {
    const user = userEvent.setup();
    let activityReads = 0;
    server.use(
      attentionRead(() => HttpResponse.json(attentionSnapshot([]))),
      activityRead(() => {
        activityReads++;
        return HttpResponse.json([noticeRow()]);
      }),
      dismissPost(async () => {
        await delay(200);
        return HttpResponse.json(
          { detail: 'Input notice changed; refresh the inbox' },
          { status: 409 }
        );
      })
    );
    renderInbox();
    await screen.findByText('Nothing needs you');
    await user.click(screen.getByRole('button', { name: 'Activity' }));

    await user.click(await screen.findByRole('button', { name: 'Dismiss notice' }));

    // The row leaves before the server answers, so the refusal arrives with no row on screen.
    await waitFor(() => expect(screen.queryByText('Input notice unavailable')).toBeNull());
    expect(await screen.findByText('Input notice changed; refresh the inbox')).toBeInTheDocument();
    // The refusal restores the row, and the settled re-read keeps it in step with the server.
    expect(await screen.findByText('Input notice unavailable')).toBeInTheDocument();
    await waitFor(() => expect(activityReads).toBeGreaterThanOrEqual(2));
  });

  it('clears the refusal once the re-read shows the row has gone', async () => {
    const user = userEvent.setup();
    let activityReads = 0;
    server.use(
      attentionRead(() => HttpResponse.json(attentionSnapshot([]))),
      activityRead(() => {
        activityReads++;
        // The first read shows the notice; the re-read after the refusal finds it already gone.
        return HttpResponse.json(activityReads === 1 ? [noticeRow()] : []);
      }),
      dismissPost(problem(409, 'Input notice changed; refresh the inbox'))
    );
    renderInbox();
    await screen.findByText('Nothing needs you');
    await user.click(screen.getByRole('button', { name: 'Activity' }));

    await user.click(await screen.findByRole('button', { name: 'Dismiss notice' }));

    expect(await screen.findByText('No activity yet')).toBeInTheDocument();
    expect(screen.queryByText('Input notice changed; refresh the inbox')).toBeNull();
    expect(screen.queryByRole('alert')).toBeNull();
  });
});

describe('Activity', () => {
  it('lists activity with a link to the task it names', async () => {
    const user = userEvent.setup();
    server.use(
      attentionRead(() => HttpResponse.json(attentionSnapshot([]))),
      activityRead(() => HttpResponse.json([activityRow()]))
    );
    renderInbox();
    await screen.findByText('Nothing needs you');

    await user.click(screen.getByRole('button', { name: 'Activity' }));

    expect(await screen.findByText('Tests passed')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'View task' })).toHaveAttribute(
      'href',
      '/tasks/task-1'
    );
  });

  it('reads activity only once the Activity view is open', async () => {
    const user = userEvent.setup();
    let activityReads = 0;
    server.use(
      attentionRead(() => HttpResponse.json(attentionSnapshot([]))),
      activityRead(() => {
        activityReads++;
        return HttpResponse.json([]);
      })
    );
    renderInbox();
    await screen.findByText('Nothing needs you');
    expect(activityReads).toBe(0);

    await user.click(screen.getByRole('button', { name: 'Activity' }));

    expect(await screen.findByText('No activity yet')).toBeInTheDocument();
    expect(activityReads).toBeGreaterThanOrEqual(1);
  });

  it('shows an error with retry when activity fails', async () => {
    const user = userEvent.setup();
    server.use(
      attentionRead(() => HttpResponse.json(attentionSnapshot([]))),
      activityRead(problem(500, 'Activity is unavailable'))
    );
    renderInbox();
    await screen.findByText('Nothing needs you');

    await user.click(screen.getByRole('button', { name: 'Activity' }));

    expect(await screen.findByText('Could not load activity.')).toBeInTheDocument();
    expect(screen.getByText('Activity is unavailable')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Try again' })).toBeInTheDocument();
  });

  it('caches activity under the key SSE invalidation uses', async () => {
    const user = userEvent.setup();
    server.use(
      attentionRead(() => HttpResponse.json(attentionSnapshot([]))),
      activityRead(() => HttpResponse.json([activityRow()]))
    );
    const { queryClient } = renderInbox();
    await screen.findByText('Nothing needs you');

    await user.click(screen.getByRole('button', { name: 'Activity' }));
    await screen.findByText('Tests passed');

    expect(queryClient.getQueryData(keys.activity())).toBeDefined();
  });
});

describe('answered requests stay in Needs you (UI-C)', () => {
  // The answered store is module-level, so one test's receipt would show up in the next.
  afterEach(() => forgetAnswered());

  it('keeps the receipt, its focus and the lead-in after the server stops listing the request', async () => {
    const user = userEvent.setup();
    const requestId = 'req-kept';
    const fake = createFakeHITL({ [requestId]: hitlView(requestId) });
    server.use(
      ...fake.handlers,
      attentionRead(() =>
        HttpResponse.json(attentionSnapshot([hitlAttentionItem(requestId, 'Approve the deploy.')]))
      )
    );
    const { queryClient } = renderInbox();
    await user.click(await screen.findByRole('button', { name: /^Approve tool\.a/ }));
    await user.click(screen.getByRole('button', { name: /^Reject tool\.b/ }));
    await user.click(screen.getByRole('button', { name: 'Send response' }));
    const receipt = await screen.findByTestId('hitl-receipt');
    expect(receipt).toHaveTextContent(/^Approved tool\.a, rejected tool\.b · \S/);
    await waitFor(() => expect(receipt).toHaveFocus());

    // The refetch drops the request and lists another, so the test can see the list re-render.
    const later = queueItem({ id: 'q-later', title: 'Which branch?' });
    server.use(attentionRead(() => HttpResponse.json(attentionSnapshot([attentionItem(later)]))));
    await act(() => queryClient.invalidateQueries({ queryKey: keys.attention() }));
    expect(await screen.findByText('Which branch?')).toBeInTheDocument();

    // The same node is still mounted and still focused: a remount would drop focus to <body>.
    expect(receipt).toBeInTheDocument();
    expect(receipt).toHaveFocus();
    expect(receipt).toHaveTextContent('Approved tool.a, rejected tool.b');
    expect(screen.getByText('Approve the deploy.')).toBeInTheDocument();
  });

  it('keeps focus and the row order when the answered request is not the first row', async () => {
    const user = userEvent.setup();
    const fake = createFakeHITL({
      'req-first': hitlView('req-first'),
      'req-second': hitlView('req-second')
    });
    server.use(
      ...fake.handlers,
      attentionRead(() =>
        HttpResponse.json(
          attentionSnapshot([
            hitlAttentionItem('req-first', 'First request.'),
            hitlAttentionItem('req-second', 'Second request.')
          ])
        )
      )
    );
    const { queryClient } = renderInbox();
    await screen.findByText('Second request.');
    const row = document.getElementById('attention-req-second') as HTMLElement;
    await user.click(await within(row).findByRole('button', { name: /^Approve tool\.a/ }));
    await user.click(within(row).getByRole('button', { name: /^Reject tool\.b/ }));
    await user.click(within(row).getByRole('button', { name: 'Send response' }));
    const receipt = await within(row).findByTestId('hitl-receipt');
    await waitFor(() => expect(receipt).toHaveFocus());

    // The refetch drops the second request and lists a third, so the test can see the list re-render.
    const third = queueItem({ id: 'q-third', title: 'Third question.' });
    server.use(
      attentionRead(() =>
        HttpResponse.json(
          attentionSnapshot([
            hitlAttentionItem('req-first', 'First request.'),
            attentionItem(third)
          ])
        )
      )
    );
    await act(() => queryClient.invalidateQueries({ queryKey: keys.attention() }));
    expect(await screen.findByText('Third question.')).toBeInTheDocument();

    expect(receipt).toHaveFocus();
    const list = screen.getByRole('list', { name: 'Needs you' });
    expect(Array.from(list.children).map((item) => item.id)).toEqual([
      'attention-req-first',
      'attention-req-second',
      'attention-q-third'
    ]);
  });
});
