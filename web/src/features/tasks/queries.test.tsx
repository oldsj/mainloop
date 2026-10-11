import type { ReactNode } from 'react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook, waitFor } from '@testing-library/react';
import { HttpResponse, type HttpResponseResolver } from 'msw';
import { expect, it, vi } from 'vitest';
import { server } from '@/test/msw/server';
import { view } from '@/test/fixtures/tasks';
import { useTask, useTaskList } from './queries';
import { taskRead, tasksList } from './test/handlers';

vi.mock('@/api/middleware', async (importOriginal) => {
  const { withFastReads } = await import('./test/fastReads');
  return withFastReads(await importOriginal());
});

it.each(['detail', 'list'] as const)(
  'cancels a %s read when its last observer unmounts',
  async (kind) => {
    let signal: AbortSignal | undefined;
    let finish!: () => void;
    const pending = new Promise<void>((resolve) => (finish = resolve));
    const read: HttpResponseResolver = async ({ request }) => {
      signal = request.signal;
      await pending;
      return HttpResponse.json(kind === 'detail' ? view() : [view()]);
    };
    server.use(kind === 'detail' ? taskRead(read) : tasksList(read));
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const wrapper = ({ children }: { children: ReactNode }) => (
      <QueryClientProvider client={client}>{children}</QueryClientProvider>
    );
    const useRead: () => void =
      kind === 'detail'
        ? () => {
            useTask('task-1');
          }
        : () => {
            useTaskList({ project_id: 'project-a' });
          };
    const mounted = renderHook(useRead, { wrapper });
    try {
      await waitFor(() => expect(signal).toBeDefined());
      mounted.unmount();
      await waitFor(() => expect(signal?.aborted).toBe(true));
    } finally {
      finish();
      client.clear();
    }
  }
);
