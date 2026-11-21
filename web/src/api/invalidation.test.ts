import { QueryClient } from '@tanstack/react-query';
import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  createEventGate,
  createSseApplier,
  planSseEvent,
  TASK_LIST_COALESCE_MS
} from './invalidation';
import { allKeys, keys } from './keys';
import { createSseParser, type SseMessage } from './sse';
import { workspaceFixture } from '@/test/fixtures';

const message = (event: string, payload: unknown = {}): Pick<SseMessage, 'event' | 'data'> => ({
  event,
  data: JSON.stringify(payload)
});

describe('planSseEvent', () => {
  it('refetches everything on a reconnect but not on the first connect', () => {
    expect(planSseEvent(message('connected'), { firstConnect: true })).toEqual([{ kind: 'none' }]);
    expect(planSseEvent(message('connected'), { firstConnect: false })).toEqual([
      { kind: 'invalidate', keys: [allKeys] }
    ]);
  });

  it('sends a progress frame to the live store and invalidates nothing', () => {
    const data = {
      session_id: 's1',
      message_id: 'm1',
      seq: 1,
      active: true,
      started_at: '2026-10-11T12:00:00',
      status: { kind: 'thinking', label: 'Thinking…' },
      text: '',
      text_truncated: false,
      tools: []
    };
    expect(planSseEvent(message('session:progress', data), { firstConnect: false })).toEqual([
      { kind: 'progress', frame: data }
    ]);
    expect(
      planSseEvent(message('session:progress', { session_id: 's1' }), { firstConnect: false })
    ).toEqual([{ kind: 'none' }]);
  });

  it('refreshes the schedule list, that schedule and its executions for schedule:updated', () => {
    expect(
      planSseEvent(message('schedule:updated', { schedule_id: 'sch-1' }), { firstConnect: false })
    ).toEqual([
      {
        kind: 'invalidate',
        keys: [keys.schedules(), keys.schedule('sch-1'), keys.scheduleExecutions('sch-1')]
      }
    ]);
  });

  it('skips a schedule:updated event that names no schedule', () => {
    expect(planSseEvent(message('schedule:updated', {}), { firstConnect: false })).toEqual([
      { kind: 'none' }
    ]);
  });

  it('refreshes attention and activity for inbox:updated', () => {
    expect(planSseEvent(message('inbox:updated'), { firstConnect: false })).toEqual([
      { kind: 'invalidate', keys: [keys.attention(), keys.activity()] }
    ]);
  });

  it('refreshes one session and the main thread for session:message', () => {
    const [action] = planSseEvent(
      message('session:message', { session_id: 's1', message_id: 'm1' }),
      {
        firstConnect: false
      }
    );
    expect(action).toEqual({
      kind: 'invalidate',
      keys: [
        keys.sessionConversation('s1'),
        keys.sessionNative('s1'),
        keys.mainThread(),
        keys.conversation()
      ]
    });
  });

  it('refreshes attention, the task lists and the task for task:updated', () => {
    expect(
      planSseEvent(message('task:updated', { event_id: 'e1', task_id: 't1', version: 2 }), {
        firstConnect: false
      })
    ).toEqual([
      { kind: 'invalidate', keys: [keys.attention()] },
      { kind: 'taskLists' },
      { kind: 'task', taskId: 't1', version: 2, eventId: 'e1' }
    ]);
  });

  it('upserts the workspace carried by workspace:updated', () => {
    const workspace = workspaceFixture({ observed_state: 'running' });
    expect(
      planSseEvent(message('workspace:updated', { workspace }), { firstConnect: false })
    ).toEqual([{ kind: 'upsertWorkspace', workspace }]);
  });

  it.each([
    ['heartbeat', {}],
    ['something:new', { a: 1 }],
    ['session:message', { nope: true }],
    ['task:updated', { task_id: 't1' }]
  ])('does nothing for %s with payload %j', (event, payload) => {
    expect(planSseEvent(message(event, payload), { firstConnect: false })).toEqual([
      { kind: 'none' }
    ]);
  });

  it('ignores a payload that is not JSON', () => {
    expect(planSseEvent({ event: 'task:updated', data: '{oops' }, { firstConnect: false })).toEqual(
      [{ kind: 'none' }]
    );
  });
});

describe('createEventGate', () => {
  it('admits an event id once and forgets the oldest past its limit', () => {
    const accept = createEventGate(2);
    expect(accept('a')).toBe(true);
    expect(accept('a')).toBe(false);
    accept('b');
    accept('c');
    expect(accept('a')).toBe(true);
  });
});

describe('createSseApplier', () => {
  const taskEvent = (version: number, eventId = `e${version}`) =>
    message('task:updated', { event_id: eventId, task_id: 't1', version });

  function setup(heldVersion?: number) {
    const queryClient = new QueryClient();
    if (heldVersion !== undefined) {
      queryClient.setQueryData(keys.task('t1'), { task: { version: heldVersion } });
    }
    return { queryClient, apply: createSseApplier(queryClient) };
  }

  it('refetches a task only when the event is newer than the held version', () => {
    const { queryClient, apply } = setup(3);
    apply(taskEvent(3));
    expect(queryClient.getQueryState(keys.task('t1'))?.isInvalidated).toBe(false);
    apply(taskEvent(4));
    expect(queryClient.getQueryState(keys.task('t1'))?.isInvalidated).toBe(true);
  });

  it('treats a malformed cached task as unknown instead of throwing', () => {
    for (const held of [{}, { task: null }, null]) {
      const queryClient = new QueryClient();
      queryClient.setQueryData(keys.task('t1'), held);
      const apply = createSseApplier(queryClient);
      expect(() => apply(taskEvent(1))).not.toThrow();
      expect(queryClient.getQueryState(keys.task('t1'))?.isInvalidated).toBe(true);
    }
  });

  it('drops a redelivered event id', () => {
    const { queryClient, apply } = setup(1);
    apply(taskEvent(2, 'same'));
    queryClient.setQueryData(keys.task('t1'), { task: { version: 1 } });
    apply(taskEvent(2, 'same'));
    expect(queryClient.getQueryState(keys.task('t1'))?.isInvalidated).toBe(false);
  });

  it('writes a workspace into the item cache and every cached variant of the list', () => {
    const { queryClient, apply } = setup();
    const starting = workspaceFixture({ observed_state: 'resuming' });
    const other = workspaceFixture({ workspace_id: 'w2', session_id: 's2' });
    const listKey = keys.workspaces();
    const filteredKey = [...listKey, { params: { query: { limit: 1 } } }];
    queryClient.setQueryData(listKey, [starting, other]);
    queryClient.setQueryData(filteredKey, [starting]);
    const running = workspaceFixture({ observed_state: 'running' });
    apply(message('workspace:updated', { workspace: running }));
    expect(queryClient.getQueryData(keys.workspace('w1'))).toEqual(running);
    expect(queryClient.getQueryData(listKey)).toEqual([running, other]);
    expect(queryClient.getQueryData(filteredKey)).toEqual([running]);
  });

  it('adds a workspace the list does not hold yet, and leaves an unloaded list alone', () => {
    const { queryClient, apply } = setup();
    const created = workspaceFixture({ workspace_id: 'w9' });
    apply(message('workspace:updated', { workspace: created }));
    expect(queryClient.getQueryData(keys.workspaces())).toBeUndefined();
    queryClient.setQueryData(keys.workspaces(), []);
    apply(message('workspace:updated', { workspace: created }));
    expect(queryClient.getQueryData(keys.workspaces())).toEqual([created]);
  });

  describe('task-list re-reads', () => {
    afterEach(() => vi.useRealTimers());

    /** How many times the task list was marked for a re-read. */
    function listReads(queryClient: QueryClient) {
      const invalidate = vi.spyOn(queryClient, 'invalidateQueries');
      return () =>
        invalidate.mock.calls.filter(
          ([filters]) => JSON.stringify(filters?.queryKey) === JSON.stringify(keys.tasks())
        ).length;
    }

    it('re-reads the list at once for one change, and once more for a burst that follows', () => {
      // Given a fleet whose tasks change in a burst.
      vi.useFakeTimers();
      const { queryClient, apply } = setup();
      const reads = listReads(queryClient);

      // When the first event arrives.
      apply(taskEvent(1));
      // Then the list is re-read straight away.
      expect(reads()).toBe(1);

      // When twenty more arrive inside the window.
      for (let version = 2; version <= 21; version++) apply(taskEvent(version));
      // Then none of them re-reads the list on its own, and each still re-reads attention.
      expect(reads()).toBe(1);
      // And one re-read at the end of the window covers them all.
      vi.advanceTimersByTime(TASK_LIST_COALESCE_MS);
      expect(reads()).toBe(2);
      vi.advanceTimersByTime(TASK_LIST_COALESCE_MS * 3);
      expect(reads()).toBe(2);
      expect(vi.getTimerCount()).toBe(0);
    });

    it('re-reads at once again when changes are further apart than the window', () => {
      vi.useFakeTimers();
      const { queryClient, apply } = setup();
      const reads = listReads(queryClient);
      apply(taskEvent(1));
      vi.advanceTimersByTime(TASK_LIST_COALESCE_MS + 1);
      apply(taskEvent(2));
      expect(reads()).toBe(2);
      // Nothing waits when no event fell inside a window.
      expect(vi.getTimerCount()).toBe(0);
    });

    it('drops a waiting re-read when the stream owner stops', () => {
      vi.useFakeTimers();
      const { queryClient, apply } = setup();
      const reads = listReads(queryClient);
      apply(taskEvent(1));
      apply(taskEvent(2));
      apply.stop();
      vi.advanceTimersByTime(TASK_LIST_COALESCE_MS * 2);
      expect(reads()).toBe(1);
    });

    it('still re-reads attention for every task event', () => {
      vi.useFakeTimers();
      const { queryClient, apply } = setup();
      const invalidate = vi.spyOn(queryClient, 'invalidateQueries');
      apply(taskEvent(1));
      apply(taskEvent(2));
      const attention = invalidate.mock.calls.filter(
        ([filters]) => JSON.stringify(filters?.queryKey) === JSON.stringify(keys.attention())
      );
      expect(attention).toHaveLength(2);
    });
  });

  it('refetches all queries on a second connect only', () => {
    const { queryClient, apply } = setup();
    queryClient.setQueryData(keys.projects(), []);
    apply(message('connected'));
    expect(queryClient.getQueryState(keys.projects())?.isInvalidated).toBe(false);
    apply(message('connected'));
    expect(queryClient.getQueryState(keys.projects())?.isInvalidated).toBe(true);
  });
});

describe('createSseParser', () => {
  it('assembles events split across chunks and keeps the last event id', () => {
    const seen: SseMessage[] = [];
    const parser = createSseParser((m) => seen.push(m));
    parser.push('id: 7\nevent: inbox:upd');
    parser.push('ated\ndata: {"a"');
    parser.push(':1}\n\n: comment\ndata: x\r\n\r\n');
    expect(seen).toEqual([
      { id: '7', event: 'inbox:updated', data: '{"a":1}' },
      { id: '7', event: 'message', data: 'x' }
    ]);
    expect(parser.lastEventId).toBe('7');
  });
});
