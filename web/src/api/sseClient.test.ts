import { describe, expect, it } from 'vitest';
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
