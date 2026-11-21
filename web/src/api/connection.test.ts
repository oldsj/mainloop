import { afterEach, describe, expect, it, vi } from 'vitest';
import { createConnection } from './connection';

describe('createConnection', () => {
  afterEach(() => vi.useRealTimers());

  it('runs one probe loop after start, stop, start (StrictMode)', async () => {
    vi.useFakeTimers();
    let resolveFirst!: (ok: boolean) => void;
    const probe = vi
      .fn<() => Promise<boolean>>()
      .mockImplementationOnce(() => new Promise((resolve) => (resolveFirst = resolve)))
      .mockResolvedValue(true);
    const connection = createConnection({ probe, onlineProbeMs: 1_000 });

    const stopFirst = connection.start();
    stopFirst();
    const stopSecond = connection.start();
    // The first run's probe is still in flight; its result must not start a second loop.
    resolveFirst(true);
    await vi.advanceTimersByTimeAsync(0);
    expect(probe).toHaveBeenCalledTimes(2);

    await vi.advanceTimersByTimeAsync(3_000);
    // One loop: a probe per second, not two.
    expect(probe).toHaveBeenCalledTimes(5);
    stopSecond();
  });

  it('ignores a stale stop function from an earlier run', async () => {
    vi.useFakeTimers();
    const probe = vi.fn(() => Promise.resolve(true));
    const connection = createConnection({ probe, onlineProbeMs: 1_000 });
    const stopFirst = connection.start();
    stopFirst();
    const stopSecond = connection.start();
    stopFirst();
    await vi.advanceTimersByTimeAsync(2_500);
    expect(probe.mock.calls.length).toBeGreaterThanOrEqual(3);
    stopSecond();
  });

  it('counts a recovery only when the probe sees the backend return', async () => {
    const probe = vi.fn<() => Promise<boolean>>().mockResolvedValueOnce(true);
    const connection = createConnection({ probe });
    await connection.check();
    expect(connection.getSnapshot().status).toBe('online');
    probe.mockResolvedValueOnce(false);
    await connection.check();
    expect(connection.getSnapshot().status).toBe('offline');
    probe.mockResolvedValueOnce(true);
    await connection.check();
    expect(connection.getSnapshot()).toMatchObject({ status: 'online', recoveries: 1 });
  });

  it('stays online when one request fails while the backend still answers', async () => {
    // Given a backend the probe can reach.
    const probe = vi.fn<() => Promise<boolean>>().mockResolvedValue(true);
    const connection = createConnection({ probe });
    await connection.check();
    const changes = vi.fn();
    connection.subscribe(changes);

    // When the API client reports a request that got no HTTP answer.
    connection.reportFailure();
    await vi.waitFor(() => expect(probe).toHaveBeenCalledTimes(2));

    // Then the probe was asked at once, and nothing reads as an outage or a recovery.
    expect(connection.getSnapshot()).toMatchObject({ status: 'online', recoveries: 0 });
    expect(changes).not.toHaveBeenCalled();
  });

  it('goes offline on a reported failure once the probe cannot reach the backend either', async () => {
    // Given a backend that was up and is now gone.
    const probe = vi.fn<() => Promise<boolean>>().mockResolvedValueOnce(true);
    const connection = createConnection({ probe });
    await connection.check();
    probe.mockResolvedValue(false);

    // When several requests fail together.
    connection.reportFailure();
    connection.reportFailure();
    connection.reportFailure();
    await vi.waitFor(() => expect(connection.getSnapshot().status).toBe('offline'));

    // Then one probe answered for all of them.
    expect(probe).toHaveBeenCalledTimes(2);
    expect(connection.getSnapshot().offlineSince).not.toBeNull();
  });
});
