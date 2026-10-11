import { QueryClient } from '@tanstack/react-query';
import { describe, expect, it } from 'vitest';
import { createEventGate, createSseApplier, planSseEvent } from './invalidation';
import { allKeys, keys } from './keys';
import { createSseParser, type SseMessage } from './sse';

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

  it('upserts the workspace carried by workspace:updated', () => {
    const workspace = { workspace_id: 'w1', lifecycle: 'running' };
    expect(
      planSseEvent(message('workspace:updated', { workspace }), { firstConnect: false })
    ).toEqual([{ kind: 'upsertWorkspace', workspaceId: 'w1', workspace }]);
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

  it('drops a redelivered event id', () => {
    const { queryClient, apply } = setup(1);
    apply(taskEvent(2, 'same'));
    queryClient.setQueryData(keys.task('t1'), { task: { version: 1 } });
    apply(taskEvent(2, 'same'));
    expect(queryClient.getQueryState(keys.task('t1'))?.isInvalidated).toBe(false);
  });

  it('writes a workspace into the item and list caches', () => {
    const { queryClient, apply } = setup();
    queryClient.setQueryData(keys.workspaces(), [{ workspace_id: 'w1', lifecycle: 'starting' }]);
    apply(
      message('workspace:updated', { workspace: { workspace_id: 'w1', lifecycle: 'running' } })
    );
    expect(queryClient.getQueryData(keys.workspace('w1'))).toEqual({
      workspace_id: 'w1',
      lifecycle: 'running'
    });
    expect(queryClient.getQueryData(keys.workspaces())).toEqual([
      { workspace_id: 'w1', lifecycle: 'running' }
    ]);
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
