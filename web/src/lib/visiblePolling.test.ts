import { expect, test } from 'vitest';
import { createVisiblePolling, singleFlight, type PollEnvironment } from './visiblePolling';

function clock(visible = true) {
  let now = 0;
  const timers = new Set<{ at: number; fn: () => void }>();
  const listeners = new Set<() => void>();
  const env: PollEnvironment = {
    visible: () => visible,
    now: () => now,
    later(fn, ms) {
      const timer = { at: now + ms, fn };
      timers.add(timer);
      return () => {
        timers.delete(timer);
      };
    },
    onVisibility(fn) {
      listeners.add(fn);
      return () => {
        listeners.delete(fn);
      };
    }
  };
  return {
    env,
    timers,
    listeners,
    hide() {
      visible = false;
      for (const fn of listeners) fn();
    },
    show() {
      visible = true;
      for (const fn of listeners) fn();
    },
    async tick(ms = 0) {
      const end = now + ms;
      for (;;) {
        const timer = [...timers].filter((t) => t.at <= end).sort((a, b) => a.at - b.at)[0];
        if (!timer) break;
        now = timer.at;
        timers.delete(timer);
        timer.fn();
        for (let i = 0; i < 10; i++) await Promise.resolve();
      }
      now = end;
      for (let i = 0; i < 10; i++) await Promise.resolve();
    }
  };
}

test('hidden/resumed/unmounted polls and staggered shared mounts', async () => {
  const time = clock(false),
    polls = createVisiblePolling(time.env);
  let reads = 0;
  const read = async () => {
    reads++;
  };
  const stopInbox = polls.watch('request', read);
  await time.tick(12000);
  expect(reads).toBe(0);
  time.show();
  await time.tick();
  expect(reads).toBe(1);
  await time.tick(1000);
  const stopChat = polls.watch('request', read);
  await time.tick(2999);
  expect(reads).toBe(1);
  await time.tick(1);
  expect(reads).toBe(2);
  stopInbox();
  await time.tick(4000);
  expect(reads).toBe(3);
  time.hide();
  await time.tick(20000);
  expect(reads).toBe(3);
  time.show();
  await time.tick();
  expect(reads).toBe(4);
  stopChat();
  await time.tick(20000);
  expect(reads).toBe(4);
  expect(time.timers.size).toBe(0);
  expect(time.listeners.size).toBe(0);
});

test('slow/abort-ignoring reads stay serialized and cancellation cannot start duplicates', async () => {
  const time = clock(),
    polls = createVisiblePolling(time.env);
  let finish!: () => void,
    reads = 0,
    signal!: AbortSignal;
  const stop = polls.watch('slow', async (s) => {
    reads++;
    signal = s;
    await new Promise<void>((resolve) => (finish = resolve));
  });
  await time.tick();
  polls.watch('other', async () => {
    reads++;
  });
  await time.tick(12000);
  expect(reads).toBe(1);
  expect(signal.aborted).toBe(true);
  time.hide();
  time.show();
  stop();
  await time.tick(12000);
  expect(reads).toBe(1);
  finish();
  await time.tick();
  await time.tick(250);
  expect(reads).toBe(2);
});

test('a large request list has a bounded start rate and delivered receipts stop', async () => {
  const time = clock(),
    polls = createVisiblePolling(time.env);
  let reads = 0;
  const stops = Array.from({ length: 100 }, (_, i) =>
    polls.watch(`request-${i}`, async () => {
      reads++;
      return false;
    })
  );
  await time.tick(999);
  expect(reads).toBe(4);
  await time.tick(25000);
  expect(reads).toBe(100);
  await time.tick(20000);
  expect(reads).toBe(100);
  stops.forEach((stop) => stop());
  expect(time.listeners.size).toBe(0);
});

test('inbox single-flight also coalesces manual/SSE callers and recovers after failure', async () => {
  let finish!: () => void,
    reads = 0;
  const read = singleFlight(async () => {
    reads++;
    await new Promise<void>((resolve) => (finish = resolve));
  });
  const first = read();
  for (let i = 0; i < 10; i++) expect(read()).toBe(first);
  expect(reads).toBe(1);
  finish();
  await first;
  const second = read();
  expect(reads).toBe(2);
  finish();
  await second;
  let attempts = 0;
  const failing = singleFlight(async () => {
    attempts++;
    throw new Error('offline');
  });
  await expect(failing()).rejects.toThrow();
  await expect(failing()).rejects.toThrow();
  expect(attempts).toBe(2);
});

test('hiding and last-unmount cancel active reads and clear queued work', async () => {
  const time = clock(),
    polls = createVisiblePolling(time.env);
  const signals: AbortSignal[] = [];
  const read = (signal: AbortSignal) =>
    new Promise<void>((resolve) => {
      signals.push(signal);
      signal.addEventListener('abort', () => resolve(), { once: true });
    });
  const stop = polls.watch('request', read);
  await time.tick();
  expect(signals.length).toBe(1);
  time.hide();
  expect(signals[0]!.aborted).toBe(true);
  await time.tick(5000);
  expect(signals.length).toBe(1);
  time.show();
  await time.tick();
  expect(signals.length).toBe(2);
  stop();
  expect(signals[1]!.aborted).toBe(true);
  await time.tick(5000);
  expect(signals.length).toBe(2);
  expect(time.timers.size).toBe(0);
  expect(time.listeners.size).toBe(0);
});
