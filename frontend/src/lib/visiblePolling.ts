/** UI reads only: one in-flight poll, shared subscriptions, and a bounded start rate. */
export interface PollEnvironment {
  visible: () => boolean;
  now: () => number;
  later: (fn: () => void, ms: number) => () => void;
  onVisibility: (fn: () => void) => () => void;
}
export function createVisiblePolling(env: PollEnvironment) {
  type Entry = {
    read: (signal: AbortSignal) => Promise<boolean | void>;
    users: number;
    due: number;
    done: boolean;
  };
  const entries = new Map<string | symbol, Entry>();
  let active: { entry: Entry; controller: AbortController } | undefined;
  let cancelTimer: (() => void) | undefined;
  let unlisten: (() => void) | undefined;
  let nextStart = 0;
  function schedule() {
    cancelTimer?.();
    cancelTimer = undefined;
    if (active || !env.visible()) return;
    const candidate = [...entries.values()].filter((e) => !e.done).sort((a, b) => a.due - b.due)[0];
    if (!candidate) return;
    cancelTimer = env.later(
      () => void run(candidate),
      Math.max(0, candidate.due - env.now(), nextStart - env.now())
    );
  }
  async function run(entry: Entry) {
    cancelTimer = undefined;
    if (active || !env.visible() || !entry.users) {
      schedule();
      return;
    }
    const controller = new AbortController();
    active = { entry, controller };
    // At most four starts/second across inbox, session lists and request cards.
    nextStart = env.now() + 250;
    entry.due = env.now() + 4000;
    const cancelDeadline = env.later(
      () => controller.abort(new DOMException('Polling read timed out', 'TimeoutError')),
      10000
    );
    try {
      if ((await entry.read(controller.signal)) === false) entry.done = true;
    } catch {
      /* The consumer owns the error state; a later visible poll can recover. */
    } finally {
      cancelDeadline();
      active = undefined;
      // Even a transport that ignores abort retains the single-flight lock until it settles.
      schedule();
    }
  }
  function visibilityChanged() {
    if (!env.visible()) {
      cancelTimer?.();
      cancelTimer = undefined;
      active?.controller.abort();
    } else {
      for (const entry of entries.values()) entry.due = 0;
      schedule();
    }
  }
  return {
    watch(key: string | symbol, read: Entry['read']) {
      let entry = entries.get(key);
      if (!entry) {
        entry = { read, users: 0, due: 0, done: false };
        entries.set(key, entry);
      }
      entry.users++;
      unlisten ??= env.onVisibility(visibilityChanged);
      schedule();
      let disposed = false;
      return () => {
        if (disposed) return;
        disposed = true;
        if (--entry.users === 0) {
          entries.delete(key);
          if (active?.entry === entry) active.controller.abort();
        }
        if (!entries.size) {
          unlisten?.();
          unlisten = undefined;
        }
        schedule();
      };
    }
  };
}

// Created lazily by mounted components, never during SSR.
let browserPolls: ReturnType<typeof createVisiblePolling> | undefined;
export function visiblePolling() {
  return (browserPolls ??= createVisiblePolling({
    visible: () => !document.hidden,
    now: () => Date.now(),
    later: (fn, ms) => {
      const timer = setTimeout(fn, ms);
      return () => clearTimeout(timer);
    },
    onVisibility: (fn) => {
      document.addEventListener('visibilitychange', fn);
      return () => document.removeEventListener('visibilitychange', fn);
    }
  }));
}

/** Also guards inbox reads triggered outside polling (SSE, layout and manual refresh). */
export function singleFlight<T extends unknown[]>(read: (...args: T) => Promise<void>) {
  let running: Promise<void> | undefined;
  return (...args: T): Promise<void> => {
    if (!running)
      running = read(...args).finally(() => {
        running = undefined;
      });
    return running;
  };
}
