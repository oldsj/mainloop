import type { ReactNode } from 'react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook } from '@testing-library/react';
import { HttpResponse, type HttpResponseResolver } from 'msw';
import { afterEach, expect, it, vi } from 'vitest';
import { fetchClient } from '@/api';
import { server } from '@/test/msw/server';
import { view } from '@/test/fixtures/tasks';
import { useTask, useTaskList } from './queries';
import { taskRead, tasksList } from './test/handlers';

vi.mock('@/api/middleware', async (importOriginal) => {
  const { withFastReads } = await import('./test/fastReads');
  return withFastReads(await importOriginal());
});

afterEach(() => vi.restoreAllMocks());

/**
 * What is checked is the signal the read hands to the API client: that is the app's part of
 * cancelling a read, and the test holds that signal itself.
 *
 * It used to watch `request.signal` inside the network handler, and failed now and then. That was
 * not timing. Between the client and the handler the request is rebuilt several times (the client,
 * the middleware, the interceptor), and under Node each rebuilt `Request` follows the one before
 * through a controller that only the `Request` object keeps alive. The handler's signal is the end
 * of that chain, so a garbage collection between the read starting and the unmount cut the chain,
 * and the abort never arrived. Forcing a collection there made the old test fail every time. A
 * browser does not have this problem; it is how Node's `Request` is built.
 *
 * Nothing here waits on a clock: the read starting is a promise, and the abort is synchronous.
 */
it.each(['detail', 'list'] as const)(
  'cancels a %s read when its last observer unmounts',
  async (kind) => {
    // The read stays in flight until the test ends, so there is something to cancel.
    let finish!: () => void;
    const pending = new Promise<void>((resolve) => (finish = resolve));
    const read: HttpResponseResolver = async () => {
      await pending;
      return HttpResponse.json(kind === 'detail' ? view() : [view()]);
    };
    server.use(kind === 'detail' ? taskRead(read) : tasksList(read));

    // Resolves with the signal of the read's call to the client, as soon as that call is made.
    const get = fetchClient.GET as (...args: unknown[]) => unknown;
    let started!: (signal: AbortSignal | undefined) => void;
    const reading = new Promise<AbortSignal | undefined>((resolve) => (started = resolve));
    vi.spyOn(fetchClient, 'GET').mockImplementation(((...args: unknown[]) => {
      started((args[1] as { signal?: AbortSignal } | undefined)?.signal);
      return get.apply(fetchClient, args);
    }) as never);

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
      const signal = await reading;
      // The read passes its signal on; without one there would be nothing to cancel it with.
      expect(signal).toBeInstanceOf(AbortSignal);
      expect(signal?.aborted).toBe(false);
      mounted.unmount();
      // Leaving the last observer cancels the read there and then.
      expect(signal?.aborted).toBe(true);
    } finally {
      finish();
      client.clear();
    }
  }
);
