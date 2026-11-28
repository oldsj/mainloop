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
import { justNow, view } from '@/test/fixtures/tasks';
import { tasksList } from '@/features/tasks/test/handlers';
import {
  activityRow,
  attentionItem,
  attentionSnapshot,
  hitlAttentionItem,
  noticeRow,
  problemAttentionItem,
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

  it('approves a privileged action with the digest it showed, and denies with a note', async () => {
    const user = userEvent.setup();
    const digest = 'a'.repeat(64);
    const bodies: unknown[] = [];
    const card = (id: string) =>
      queueItem({
        id,
        item_type: 'approval',
        title: `Merge octo-org/widgets#${id}`,
        options: ['Approve', 'Deny'],
        context: { privileged_action_digest: digest }
      });
    server.use(
      attentionRead(() =>
        HttpResponse.json(attentionSnapshot([attentionItem(card('7')), attentionItem(card('8'))]))
      ),
      respondPost(async ({ request, params }) => {
        bodies.push([params.item_id, await request.json()]);
        return HttpResponse.json({ status: 'ok', message: 'Sent' });
      })
    );
    renderInbox();

    const first = (await screen.findByText('Merge octo-org/widgets#7')).closest('article')!;
    expect(
      within(first as HTMLElement).getByText(
        'Approve here is recorded as approved from the web app (no Touch ID).'
      )
    ).toBeInTheDocument();
    await user.click(within(first as HTMLElement).getByRole('button', { name: 'Approve' }));
    await waitFor(() =>
      expect(bodies).toEqual([
        ['7', { response: 'Approve', metadata: { reviewed_context: digest } }]
      ])
    );

    const second = screen.getByText('Merge octo-org/widgets#8').closest('article')!;
    await user.type(
      within(second as HTMLElement).getByPlaceholderText('Deny with a note...'),
      'Wait for the release'
    );
    await user.click(within(second as HTMLElement).getByRole('button', { name: 'Send' }));
    await waitFor(() => expect(bodies).toHaveLength(2));
    expect(bodies[1]).toEqual(['8', { response: 'Wait for the release' }]);
  });

  it('in the iPhone app, approves a privileged action through the app, never unsigned', async () => {
    const user = userEvent.setup();
    const digest = 'b'.repeat(64);
    const posted: unknown[] = [];
    const replies: unknown[] = [
      { status: 'cancelled' },
      { status: 'failed', message: 'Touch ID failed.' },
      { status: 'approved' }
    ];
    (window as unknown as { webkit: unknown }).webkit = {
      messageHandlers: {
        mainloop: {
          postMessage: async (message: unknown) => {
            posted.push(message);
            return replies.shift();
          }
        }
      }
    };
    const bodies: unknown[] = [];
    server.use(
      attentionRead(() =>
        HttpResponse.json(
          attentionSnapshot([
            attentionItem(
              queueItem({
                id: '9',
                item_type: 'approval',
                title: 'Restart shop/web',
                options: ['Approve', 'Deny'],
                context: { privileged_action_digest: digest, privileged_action_id: 'pa-0009' }
              })
            )
          ])
        )
      ),
      respondPost(async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json({ status: 'ok', message: 'Sent' });
      })
    );
    try {
      renderInbox();
      const card = (await screen.findByText('Restart shop/web')).closest('article') as HTMLElement;
      const approve = () => user.click(within(card).getByRole('button', { name: 'Approve' }));
      expect(within(card).getByText(/Approve here signs with Touch ID/)).toBeInTheDocument();

      await approve(); // Touch ID cancelled: nothing shown, nothing sent
      await waitFor(() => expect(posted).toHaveLength(1));
      expect(within(card).queryByRole('alert')).not.toBeInTheDocument();

      await approve();
      expect(await within(card).findByRole('alert')).toHaveTextContent('Touch ID failed.');

      await approve();
      expect(await within(card).findByText('> Approve')).toBeInTheDocument();
      expect(posted).toEqual(
        Array(3).fill({ type: 'approve', item_id: '9', action_id: 'pa-0009', digest })
      );
      expect(bodies).toEqual([]); // the page never sent an Approve itself
    } finally {
      delete (window as unknown as { webkit?: unknown }).webkit;
    }
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

  it('keeps a problem (an unresolved runtime stop) out of Needs you', async () => {
    const row = problemAttentionItem({
      id: 'reconciliation:task-9',
      reason_code: 'runtime_stop_unresolved',
      task_id: 'task-9',
      title: 'Recover stopped child'
    });
    server.use(attentionRead(() => HttpResponse.json(attentionSnapshot([row]))));
    renderInbox();
    expect(await screen.findByText('Nothing needs you')).toBeInTheDocument();
    expect(screen.queryByText('Recover stopped child')).not.toBeInTheDocument();
  });

  it('lists a project memory review as a decision and links it to the project page', async () => {
    const review = attentionItem(queueItem(), {
      id: 'memory:project-a',
      kind: 'review',
      queue_item: null,
      task_id: null,
      href: '/projects/project-a',
      title: 'oldsj/mainloop',
      needed: '2 project memory proposals wait for you. Accept or reject them.'
    });
    server.use(attentionRead(() => HttpResponse.json(attentionSnapshot([review]))));
    renderInbox();
    expect(
      await screen.findByText('2 project memory proposals wait for you. Accept or reject them.')
    ).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Review on the project page' })).toHaveAttribute(
      'href',
      '/projects/project-a#project-memory'
    );
  });

  it('lists the owner memory review as a decision and links it to Settings', async () => {
    const review = attentionItem(queueItem(), {
      id: 'owner-memory:dev-user',
      kind: 'review',
      queue_item: null,
      task_id: null,
      href: '/settings#memory',
      title: 'Owner memory',
      needed: '2 owner memory proposals and 1 entry to review.'
    });
    server.use(attentionRead(() => HttpResponse.json(attentionSnapshot([review]))));
    renderInbox();
    expect(
      await screen.findByText('2 owner memory proposals and 1 entry to review.')
    ).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Review in Settings' })).toHaveAttribute(
      'href',
      '/settings#memory'
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
});

describe('tasks that need a decision without an item of their own', () => {
  it('lists the task after the items and counts it, so the badge matches the rows', async () => {
    const question = attentionItem(queueItem({ id: 'q1', title: 'Which branch?' }));
    server.use(
      attentionRead(() => HttpResponse.json(attentionSnapshot([question]))),
      tasksList(() =>
        HttpResponse.json([
          view({ id: 'task-f', title: 'Fix CI flake', status: 'failed', updated_at: justNow() })
        ])
      )
    );
    renderInbox();
    const row = await screen.findByTestId('inbox-task-row');
    expect(within(row).getByText('Fix CI flake')).toBeInTheDocument();
    expect(within(row).getByText('Failed')).toBeInTheDocument();
    expect(within(row).getByRole('link', { name: 'Review task and decide' })).toHaveAttribute(
      'href',
      '/tasks/task-f'
    );
    const list = screen.getByRole('list', { name: 'Needs you' });
    expect(within(list).getAllByRole('listitem')).toHaveLength(2);
    expect(screen.getByRole('button', { name: /^Needs you/ })).toHaveTextContent('Needs you2');
  });

  it('does not say nothing needs the owner when only a task does', async () => {
    server.use(
      attentionRead(() => HttpResponse.json(attentionSnapshot([]))),
      tasksList(() =>
        HttpResponse.json([
          view({ id: 'task-f', title: 'Fix CI flake', status: 'failed', updated_at: justNow() })
        ])
      )
    );
    renderInbox();
    expect(await screen.findByTestId('inbox-task-row')).toBeInTheDocument();
    expect(screen.queryByText('Nothing needs you')).toBeNull();
  });

  it('lists a task Mainloop is recovering under Problems, with its count', async () => {
    const user = userEvent.setup();
    server.use(
      attentionRead(() => HttpResponse.json(attentionSnapshot([]))),
      tasksList(() =>
        HttpResponse.json([
          view({ id: 'task-s', title: 'Sync mirrors', status: 'blocked', reason: 'stalled' })
        ])
      )
    );
    renderInbox();
    const tab = await screen.findByRole('button', { name: /^Problems1/ });
    await user.click(tab);
    const row = await screen.findByTestId('inbox-task-row');
    expect(within(row).getByText('Sync mirrors')).toBeInTheDocument();
    expect(within(row).getByRole('link', { name: 'Review task evidence' })).toHaveAttribute(
      'href',
      '/tasks/task-s'
    );
  });
});

describe('Problems', () => {
  it('links an unresolved runtime stop to task evidence, not a decision', async () => {
    const user = userEvent.setup();
    const row = problemAttentionItem({
      id: 'reconciliation:task-9',
      reason_code: 'runtime_stop_unresolved',
      task_id: 'task-9',
      title: 'Recover stopped child'
    });
    server.use(attentionRead(() => HttpResponse.json(attentionSnapshot([row]))));
    renderInbox();
    await screen.findByText('Nothing needs you');
    // The tab carries a "1" badge for the one problem, so its accessible name is "Problems1".
    await user.click(screen.getByRole('button', { name: /^Problems/ }));
    expect(await screen.findByText('Recover stopped child')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Review task evidence' })).toHaveAttribute(
      'href',
      '/tasks/task-9'
    );
  });

  it('shows the empty state when Mainloop is handling nothing', async () => {
    const user = userEvent.setup();
    server.use(attentionRead(() => HttpResponse.json(attentionSnapshot([]))));
    renderInbox();
    await screen.findByText('Nothing needs you');
    await user.click(screen.getByRole('button', { name: 'Problems' }));
    expect(await screen.findByText('Nothing here')).toBeInTheDocument();
  });
});

describe('Needs you refresh and selection', () => {
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
