import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import { HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { server } from '@/test/msw/server';
import { InboxBadge } from './InboxBadge';
import { attentionSnapshot } from './test/fixtures';
import { activityRead, attentionRead } from './test/handlers';

function renderBadge() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <InboxBadge />
    </QueryClientProvider>
  );
  return client;
}

describe('inbox badge', () => {
  it('shows the unresolved count', async () => {
    server.use(attentionRead(() => HttpResponse.json(attentionSnapshot([], 2))));
    renderBadge();
    expect(await screen.findByTestId('inbox-badge')).toHaveTextContent('2');
  });

  it('caps the count at 99+', async () => {
    server.use(attentionRead(() => HttpResponse.json(attentionSnapshot([], 150))));
    renderBadge();
    expect(await screen.findByTestId('inbox-badge')).toHaveTextContent('99+');
  });

  it('shows nothing when nothing needs the owner', async () => {
    let reads = 0;
    server.use(
      attentionRead(() => {
        reads++;
        return HttpResponse.json(attentionSnapshot([], 0));
      })
    );
    renderBadge();
    await waitFor(() => expect(reads).toBe(1));
    expect(screen.queryByTestId('inbox-badge')).toBeNull();
  });

  it('never reads or counts activity', async () => {
    let activityReads = 0;
    server.use(
      attentionRead(() => HttpResponse.json(attentionSnapshot([], 1))),
      activityRead(() => {
        activityReads++;
        return HttpResponse.json([
          {
            id: 'report:1',
            title: 't',
            content: 'c',
            href: null,
            created_at: '2026-10-11T09:00:00Z'
          }
        ]);
      })
    );
    renderBadge();
    expect(await screen.findByTestId('inbox-badge')).toHaveTextContent('1');
    expect(activityReads).toBe(0);
  });
});
