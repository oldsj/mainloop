import assert from 'node:assert/strict';
import test from 'node:test';
import type { TaskView } from './api';
import { view } from './taskFixtures.ts';
import {
  createEventGate,
  createReadCoalescer,
  createReconnectWatcher,
  createTaskSync,
  mergeView,
  needsRead,
  parseTaskEvent
} from './taskEvents.ts';

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
  assert.equal(parseTaskEvent(null), null);
  assert.equal(parseTaskEvent('x'), null);
  assert.equal(parseTaskEvent({ task_id: 't', version: 1 }), null);
  assert.equal(parseTaskEvent(event({ version: 'four' })), null);
  assert.equal(parseTaskEvent(event({ version: Number.NaN })), null);
  assert.equal(parseTaskEvent(event())?.task_id, 'task-1');
});

test('an event gate drops redelivered event IDs', () => {
  const gate = createEventGate();
  assert.equal(gate.accept('a'), true);
  assert.equal(gate.accept('a'), false);
  assert.equal(gate.accept('b'), true);
});

test('an event gate forgets only the oldest IDs past its limit', () => {
  const gate = createEventGate(2);
  gate.accept('a');
  gate.accept('b');
  gate.accept('c');
  assert.equal(gate.accept('c'), false);
  assert.equal(gate.accept('a'), true);
});

test('only a newer version, or an unknown task, needs a read', () => {
  const e = parseTaskEvent(event({ version: 4 }))!;
  assert.equal(needsRead(undefined, e), true);
  assert.equal(needsRead(3, e), true);
  assert.equal(needsRead(4, e), false);
  assert.equal(needsRead(5, e), false);
});

test('a slow older read never rolls the screen back', () => {
  const held = view({ version: 5, status: 'blocked' });
  const older = view({ version: 4, status: 'running' });
  assert.equal(mergeView(held, older), held);
  const newer = view({ version: 6, status: 'completed' });
  assert.equal(mergeView(held, newer), newer);
  assert.equal(mergeView(undefined, older), older);
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
  assert.equal(calls, 1);
  releases[0]();
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(calls, 2);
  releases[1]();
  await first;
  assert.equal(calls, 2);
});

test('only connections after the first are reconcile points', () => {
  const watcher = createReconnectWatcher();
  assert.equal(watcher.connected(), false);
  assert.equal(watcher.connected(), true);
  watcher.reset();
  assert.equal(watcher.connected(), false);
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
  assert.deepEqual(h.fetches, ['task-1']);
  assert.equal(h.held.get('task-1')?.task.version, 4);
});

test('an event for a version already held does not read', async () => {
  const h = harness([view({ version: 4 })]);
  await h.sync.handleEvent(event({ event_id: 'evt-a', version: 4 }));
  await h.sync.handleEvent(event({ event_id: 'evt-b', version: 2 }));
  assert.deepEqual(h.fetches, []);
});

test('an event for an unseen task reads it', async () => {
  const h = harness();
  h.server.set('task-1', view({ version: 1 }));
  await h.sync.handleEvent(event({ version: 1 }));
  assert.equal(h.held.get('task-1')?.task.version, 1);
});

test('the event itself is never trusted as state: the GET result is what is stored', async () => {
  const h = harness([view({ version: 3, status: 'running' })]);
  h.server.set('task-1', view({ version: 7, status: 'blocked', reason: 'handoff' }));
  await h.sync.handleEvent(event({ version: 4 }));
  assert.equal(h.held.get('task-1')?.task.version, 7);
  assert.equal(h.held.get('task-1')?.task.status, 'blocked');
});

test('a reconnect re-reads every held task and the lists; the first connection does not', async () => {
  const h = harness([view({ id: 'task-1', version: 3 }), view({ id: 'task-2', version: 3 })]);
  h.server.set('task-1', view({ id: 'task-1', version: 5 }));
  h.server.set('task-2', view({ id: 'task-2', version: 3 }));

  await h.sync.handleConnected();
  assert.deepEqual(h.fetches, []);
  assert.equal(h.listReads(), 0);

  // Events published while the stream was down are gone; the GET recovers the change.
  await h.sync.handleConnected();
  assert.deepEqual(h.fetches.sort(), ['task-1', 'task-2']);
  assert.equal(h.listReads(), 1);
  assert.equal(h.held.get('task-1')?.task.version, 5);
});

test('a connection already open when listening starts makes the next connect a reconcile', async () => {
  const h = harness([view({ version: 3 })]);
  h.server.set('task-1', view({ version: 4 }));
  h.sync.assumeConnected();
  await h.sync.handleConnected();
  assert.deepEqual(h.fetches, ['task-1']);
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
  assert.equal(held.get('task-1')?.task.version, 3);
  assert.deepEqual(errors, ['task-1']);
});
