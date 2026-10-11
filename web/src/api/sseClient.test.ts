import { afterEach, describe, expect, it, vi } from 'vitest';
import { createSseClient, type SseMessage } from './sse';

const stream = (text: string) =>
  new Response(text, { headers: { 'Content-Type': 'text/event-stream' } });

describe('createSseClient', () => {
  it('reconnects with Last-Event-ID from the last event it saw', async () => {
    const requests: Array<string | null> = [];
    const messages: SseMessage[] = [];
    let client!: ReturnType<typeof createSseClient>;
    const done = new Promise<void>((resolve) => {
      const fetchStub: typeof fetch = (_url, init) => {
        const headers = new Headers(init?.headers);
        requests.push(headers.get('Last-Event-ID'));
        if (requests.length === 1)
          return Promise.resolve(stream('id: 41\nevent: inbox:updated\ndata: {}\n\n'));
        resolve();
        return Promise.resolve(stream(''));
      };
      client = createSseClient({
        url: 'http://api.test/events',
        fetch: fetchStub,
        backoff: () => 0,
        sleep: () => Promise.resolve(),
        onMessage: (m) => messages.push(m)
      });
      client.start();
    });
    await done;
    client.stop();
    expect(requests).toEqual([null, '41']);
    expect(messages.map((m) => m.event)).toEqual(['inbox:updated']);
  });
});

describe('createSseClient watchdog and wake-ups', () => {
  afterEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  /** A stream the test feeds by hand; `fetch` calls are counted. */
  function silentServer() {
    const controllers: ReadableStreamDefaultController<Uint8Array>[] = [];
    const encoder = new TextEncoder();
    const fetchStub = vi.fn<typeof fetch>((_url, init) => {
      const body = new ReadableStream<Uint8Array>({
        start(controller) {
          controllers.push(controller);
          init?.signal?.addEventListener('abort', () => {
            try {
              controller.close();
            } catch {
              // already closed
            }
          });
        }
      });
      return Promise.resolve(
        new Response(body, { headers: { 'Content-Type': 'text/event-stream' } })
      );
    });
    return {
      fetchStub,
      send: (i: number, text: string) => controllers[i]?.enqueue(encoder.encode(text))
    };
  }

  it('reconnects when no bytes arrive for 75 s', async () => {
    vi.useFakeTimers();
    const { fetchStub } = silentServer();
    const client = createSseClient({
      fetch: fetchStub,
      backoff: () => 0,
      sleep: () => Promise.resolve(),
      onMessage: () => {}
    });
    client.start();
    await vi.advanceTimersByTimeAsync(74_000);
    expect(fetchStub).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(2_000);
    expect(fetchStub).toHaveBeenCalledTimes(2);
    client.stop();
  });

  it('counts any bytes, including a comment line, as activity', async () => {
    vi.useFakeTimers();
    const { fetchStub, send } = silentServer();
    const client = createSseClient({
      fetch: fetchStub,
      backoff: () => 0,
      sleep: () => Promise.resolve(),
      onMessage: () => {}
    });
    client.start();
    await vi.advanceTimersByTimeAsync(60_000);
    send(0, ': keep-alive\n\n');
    await vi.advanceTimersByTimeAsync(60_000);
    expect(fetchStub).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(20_000);
    expect(fetchStub).toHaveBeenCalledTimes(2);
    client.stop();
  });

  function failingFetch() {
    return vi.fn<typeof fetch>(() => Promise.reject(new TypeError('Failed to fetch')));
  }

  it('cuts the backoff short when the network comes back', async () => {
    vi.useFakeTimers();
    const fetchStub = failingFetch();
    const client = createSseClient({
      fetch: fetchStub,
      backoff: () => 60_000,
      onMessage: () => {}
    });
    client.start();
    await vi.advanceTimersByTimeAsync(1_000);
    expect(fetchStub).toHaveBeenCalledTimes(1);
    window.dispatchEvent(new Event('online'));
    await vi.advanceTimersByTimeAsync(0);
    expect(fetchStub).toHaveBeenCalledTimes(2);
    client.stop();
  });

  it('cuts the backoff short when the tab becomes visible, not when it is hidden', async () => {
    vi.useFakeTimers();
    const visibility = vi.spyOn(document, 'visibilityState', 'get');
    const fetchStub = failingFetch();
    const client = createSseClient({
      fetch: fetchStub,
      backoff: () => 60_000,
      onMessage: () => {}
    });
    client.start();
    await vi.advanceTimersByTimeAsync(1_000);
    visibility.mockReturnValue('hidden');
    document.dispatchEvent(new Event('visibilitychange'));
    await vi.advanceTimersByTimeAsync(0);
    expect(fetchStub).toHaveBeenCalledTimes(1);
    visibility.mockReturnValue('visible');
    document.dispatchEvent(new Event('visibilitychange'));
    await vi.advanceTimersByTimeAsync(0);
    expect(fetchStub).toHaveBeenCalledTimes(2);
    client.stop();
  });

  it('does not leak listeners across backoff waits', async () => {
    vi.useFakeTimers();
    const live = new Map<string, number>();
    const track = (target: EventTarget, label: string) => {
      const add = target.addEventListener.bind(target);
      const remove = target.removeEventListener.bind(target);
      const bump = (type: string, delta: number) =>
        live.set(`${label}:${type}`, (live.get(`${label}:${type}`) ?? 0) + delta);
      vi.spyOn(target, 'addEventListener').mockImplementation((type, ...rest) => {
        bump(type, 1);
        return add(type, ...rest);
      });
      vi.spyOn(target, 'removeEventListener').mockImplementation((type, ...rest) => {
        bump(type, -1);
        return remove(type, ...rest);
      });
    };
    track(window, 'window');
    track(document, 'document');
    track(AbortSignal.prototype, 'signal');
    const client = createSseClient({
      fetch: failingFetch(),
      backoff: () => 1_000,
      onMessage: () => {}
    });
    client.start();
    // Several connect-and-wait cycles; we are inside a wait now.
    await vi.advanceTimersByTimeAsync(5_500);
    // Only the current wait's own listeners may be registered, not one per past wait.
    expect(live.get('signal:abort')).toBe(1);
    expect(live.get('window:online')).toBe(1);
    expect(live.get('document:visibilitychange')).toBe(1);
    client.stop();
  });
});
