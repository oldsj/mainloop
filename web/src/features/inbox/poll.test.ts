import { QueryClient } from '@tanstack/react-query';
import { HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { keys } from '@/api';
import { server } from '@/test/msw/server';
import { readActivity, readAttention } from './poll';
import { activityRow, attentionSnapshot } from './test/fixtures';
import { activityRead, attentionRead, problem } from './test/handlers';

const newClient = () =>
  new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });

describe('attention poll read', () => {
  it('resolves without a value when the read fails, so polling keeps going', async () => {
    server.use(attentionRead(problem(500, 'Backend unavailable')));
    const client = newClient();

    await expect(readAttention(client, new AbortController().signal)).resolves.toBeUndefined();
    expect(client.getQueryState(keys.attention())?.status).toBe('error');
  });

  it('stores the snapshot it reads', async () => {
    server.use(attentionRead(() => HttpResponse.json(attentionSnapshot([], 4))));
    const client = newClient();

    await readAttention(client, new AbortController().signal);
    expect(client.getQueryData(keys.attention())).toEqual(attentionSnapshot([], 4));
  });

  it('sends nothing once the poll is already aborted', async () => {
    let reads = 0;
    server.use(
      attentionRead(() => {
        reads++;
        return HttpResponse.json(attentionSnapshot([]));
      })
    );
    const controller = new AbortController();
    controller.abort();

    await readAttention(newClient(), controller.signal);
    expect(reads).toBe(0);
  });
});

describe('activity poll read', () => {
  it('stores the rows it reads', async () => {
    server.use(activityRead(() => HttpResponse.json([activityRow()])));
    const client = newClient();

    await readActivity(client, new AbortController().signal);
    expect(client.getQueryData(keys.activity())).toEqual([activityRow()]);
  });
});
