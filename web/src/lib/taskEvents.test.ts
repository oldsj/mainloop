import { expect, test } from 'vitest';
import type { TaskView } from './contract';
import { view } from '../test/fixtures/tasks';
import {
  createEventGate,
  createReadCoalescer,
  createReconnectWatcher,
  createTaskSync,
  mergeView,
  needsRead,
  parseTaskEvent
} from './taskEvents';

const event = (overrides: Record<string, unknown> = {}) => ({
  type: 'task:updated',
  event_id: 'evt-1',
  task_id: 'task-1',
  version: 4,
  attempt_id: 'attempt-1',
  root_task_id: 'task-1',
  parent_task_id: null,
  occurred_at: '2026-01-01T12:00:00Z',
  ...overrides
});

test('malformed events are ignored', () => {
  expect(parseTaskEvent(null)).toBe(null);
  expect(parseTaskEvent('x')).toBe(null);
  expect(parseTaskEvent({ task_id: 't', version: 1 })).toBe(null);
  expect(parseTaskEvent(event({ version: 'four' }))).toBe(null);
  expect(parseTaskEvent(event({ version: Number.NaN }))).toBe(null);
  expect(parseTaskEvent(event())?.task_id).toBe('task-1');
});

test('an event gate drops redelivered event IDs', () => {
  const gate = createEventGate();
  expect(gate.accept('a')).toBe(true);
  expect(gate.accept('a')).toBe(false);
  expect(gate.accept('b')).toBe(true);
});

test('an event gate forgets only the oldest IDs past its limit', () => {
  const gate = createEventGate(2);
  gate.accept('a');
  gate.accept('b');
  gate.accept('c');
  expect(gate.accept('c')).toBe(false);
  expect(gate.accept('a')).toBe(true);
});

test('only a newer version, or an unknown task, needs a read', () => {
  const e = parseTaskEvent(event({ version: 4 }))!;
  expect(needsRead(undefined, e)).toBe(true);
  expect(needsRead(3, e)).toBe(true);
  expect(needsRead(4, e)).toBe(false);
  expect(needsRead(5, e)).toBe(false);
});

test('a slow older read never rolls the screen back', () => {
  const held = view({ version: 5, status: 'blocked' });
  const older = view({ version: 4, status: 'running' });
  expect(mergeView(held, older)).toBe(held);
  const newer = view({ version: 6, status: 'completed' });
  expect(mergeView(held, newer)).toBe(newer);
  expect(mergeView(undefined, older)).toBe(older);
});

test('concurrent reads of one task coalesce into one follow-up', async () => {
  let calls = 0;
  const releases: (() => void)[] = [];
  const reads = createReadCoalescer(
    () =>
      new Promise<void>((resolve) => {
        calls += 1;
        releases.push(resolve);
      })
  );
  const first = reads.request('t');
  void reads.request('t');
  void reads.request('t');
  expect(calls).toBe(1);
  releases[0]!();
  await new Promise((resolve) => setTimeout(resolve, 0));
  expect(calls).toBe(2);
  releases[1]!();
  await first;
  expect(calls).toBe(2);
});

test('only connections after the first are reconcile points', () => {
  const watcher = createReconnectWatcher();
  expect(watcher.connected()).toBe(false);
  expect(watcher.connected()).toBe(true);
  watcher.reset();
  expect(watcher.connected()).toBe(false);
});

function harness(initial: TaskView[] = []) {
  const held = new Map(initial.map((v) => [v.task.id, v]));
  const server = new Map(initial.map((v) => [v.task.id, v]));
  const fetches: string[] = [];
  let listReads = 0;
  const sync = createTaskSync({
    held: (id) => held.get(id),
    heldIds: () => [...held.keys()],
    fetchTask: async (id) => {
      fetches.push(id);
      const found = server.get(id);
      if (!found) throw new Error('not found');
      return found;
    },
    apply: (v) => held.set(v.task.id, v),
    reconcileLists: async () => {
      listReads += 1;
    }
  });
  return { held, server, fetches, sync, listReads: () => listReads };
}

test('a duplicate task:updated event triggers one GET', async () => {
  const h = harness([view({ version: 3 })]);
  h.server.set('task-1', view({ version: 4 }));
  await h.sync.handleEvent(event({ event_id: 'evt-9', version: 4 }));
  await h.sync.handleEvent(event({ event_id: 'evt-9', version: 4 }));
  expect(h.fetches).toStrictEqual(['task-1']);
  expect(h.held.get('task-1')?.task.version).toBe(4);
});

test('an event for a version already held does not read', async () => {
  const h = harness([view({ version: 4 })]);
  await h.sync.handleEvent(event({ event_id: 'evt-a', version: 4 }));
  await h.sync.handleEvent(event({ event_id: 'evt-b', version: 2 }));
  expect(h.fetches).toStrictEqual([]);
});

test('an event for an unseen task reads it', async () => {
  const h = harness();
  h.server.set('task-1', view({ version: 1 }));
  await h.sync.handleEvent(event({ version: 1 }));
  expect(h.held.get('task-1')?.task.version).toBe(1);
});

test('the event itself is never trusted as state: the GET result is what is stored', async () => {
  const h = harness([view({ version: 3, status: 'running' })]);
  h.server.set('task-1', view({ version: 7, status: 'blocked', reason: 'handoff' }));
  await h.sync.handleEvent(event({ version: 4 }));
  expect(h.held.get('task-1')?.task.version).toBe(7);
  expect(h.held.get('task-1')?.task.status).toBe('blocked');
});

test('a reconnect re-reads every held task and the lists; the first connection does not', async () => {
  const h = harness([view({ id: 'task-1', version: 3 }), view({ id: 'task-2', version: 3 })]);
  h.server.set('task-1', view({ id: 'task-1', version: 5 }));
  h.server.set('task-2', view({ id: 'task-2', version: 3 }));

  await h.sync.handleConnected();
  expect(h.fetches).toStrictEqual([]);
  expect(h.listReads()).toBe(0);

  // Events published while the stream was down are gone; the GET recovers the change.
  await h.sync.handleConnected();
  expect(h.fetches.sort()).toStrictEqual(['task-1', 'task-2']);
  expect(h.listReads()).toBe(1);
  expect(h.held.get('task-1')?.task.version).toBe(5);
});

test('a connection already open when listening starts makes the next connect a reconcile', async () => {
  const h = harness([view({ version: 3 })]);
  h.server.set('task-1', view({ version: 4 }));
  h.sync.assumeConnected();
  await h.sync.handleConnected();
  expect(h.fetches).toStrictEqual(['task-1']);
});

test('a failed read keeps the held view and reports the error', async () => {
  const errors: string[] = [];
  const held = new Map([['task-1', view({ version: 3 })]]);
  const sync = createTaskSync({
    held: (id) => held.get(id),
    heldIds: () => [...held.keys()],
    fetchTask: async () => {
      throw new Error('backend down');
    },
    apply: (v) => held.set(v.task.id, v),
    reconcileLists: async () => {},
    onError: (id) => errors.push(id)
  });
  await sync.handleEvent(event({ version: 4 }));
  expect(held.get('task-1')?.task.version).toBe(3);
  expect(errors).toStrictEqual(['task-1']);
});
