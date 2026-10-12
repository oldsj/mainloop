import { describe, expect, it } from 'vitest';
import type { TaskView } from '@/lib/contract';
import { NOW, projection, view } from '@/test/fixtures/tasks';
import { countText, groupWork, relativeTime, sinceCounts, workGroup } from './model';

const MIN = 60_000;
const at = (minutesBeforeNow: number) => new Date(NOW - minutesBeforeNow * MIN).toISOString();
const LAST_VISIT = NOW - 60 * MIN; // the owner was last here an hour ago

const ids = (rows: { view: TaskView }[]) => rows.map((row) => row.view.task.id);
const viewIds = (views: TaskView[]) => views.map((view) => view.task.id);

describe('workGroup', () => {
  it('counts a running task as current work, never as idle', () => {
    expect(workGroup(view({ id: 'run', status: 'running' }), LAST_VISIT)).toBe('working');
    expect(workGroup(view({ id: 'q', status: 'queued' }), LAST_VISIT)).toBe('working');
    expect(workGroup(view({ id: 'w', status: 'waiting', reason: 'ci' }), LAST_VISIT)).toBe(
      'working'
    );
  });

  it('puts a task that waits on the owner in Needs you, even when it is failed', () => {
    const waiting = view({ id: 'a', status: 'waiting', reason: 'owner_decision' });
    const pendingApproval = view(
      { id: 'b', status: 'running' },
      { projection: projection({ pending_approval_ids: ['req-1'] }) }
    );
    const failedButAsking = view({ id: 'c', status: 'failed', reason: 'approval' });
    expect(workGroup(waiting, LAST_VISIT)).toBe('needs-you');
    expect(workGroup(pendingApproval, LAST_VISIT)).toBe('needs-you');
    expect(workGroup(failedButAsking, LAST_VISIT)).toBe('needs-you');
  });

  it('puts failed and blocked tasks in Blocked or failed', () => {
    expect(workGroup(view({ status: 'failed' }), LAST_VISIT)).toBe('blocked');
    expect(workGroup(view({ status: 'blocked' }), LAST_VISIT)).toBe('blocked');
  });

  it('shows a finished task only when it finished after the last visit', () => {
    const recent = view({ status: 'completed', updated_at: at(10) });
    const old = view({ status: 'completed', updated_at: at(600) });
    expect(workGroup(recent, LAST_VISIT)).toBe('finished');
    expect(workGroup(old, LAST_VISIT)).toBeNull();
  });

  it('shows no finished task when there is no visit on record', () => {
    expect(workGroup(view({ status: 'completed', updated_at: at(1) }), null)).toBeNull();
  });
});

describe('groupWork', () => {
  it('puts each task in one group and orders each group newest first', () => {
    const views = [
      view({ id: 'old-run', status: 'running', updated_at: at(90) }),
      view({ id: 'new-run', status: 'running', updated_at: at(2) }),
      view({ id: 'asks', status: 'waiting', reason: 'approval', updated_at: at(30) }),
      view({ id: 'broke', status: 'failed', updated_at: at(5) }),
      view({ id: 'done', status: 'completed', updated_at: at(1) }),
      view({ id: 'stale', status: 'completed', updated_at: at(900) })
    ];
    const groups = groupWork(views, LAST_VISIT);
    expect(ids(groups['needs-you'])).toEqual(['asks']);
    expect(ids(groups.working)).toEqual(['new-run', 'old-run']);
    expect(ids(groups.blocked)).toEqual(['broke']);
    expect(ids(groups.finished)).toEqual(['done']);
  });

  it('gives each row a one-line status with its reason', () => {
    const [row] = groupWork([view({ status: 'waiting', reason: 'ci' })], LAST_VISIT).working;
    expect(row?.statusLine).toBe('Waiting · awaiting CI');
  });

  it('returns empty groups, not missing ones, for an empty list', () => {
    expect(groupWork([], LAST_VISIT)).toEqual({
      'needs-you': [],
      working: [],
      blocked: [],
      finished: []
    });
  });
});

describe('sinceCounts', () => {
  it('counts merged, failed and waiting tasks that changed since the visit, newest first', () => {
    const views = [
      view(
        { id: 'merged-new', status: 'completed', updated_at: at(5) },
        {
          projection: projection({ merge_state: 'merged', pr_state: 'merged' })
        }
      ),
      view(
        { id: 'merged-old', status: 'completed', updated_at: at(20) },
        {
          projection: projection({ merge_state: 'merged', pr_state: 'merged' })
        }
      ),
      view({ id: 'failed', status: 'failed', updated_at: at(10) }),
      view({ id: 'asks', status: 'waiting', reason: 'owner_decision', updated_at: at(15) }),
      view({ id: 'old-failed', status: 'failed', updated_at: at(300) })
    ];
    const counts = sinceCounts(views, LAST_VISIT);
    expect(viewIds(counts.merged)).toEqual(['merged-new', 'merged-old']);
    expect(viewIds(counts.failed)).toEqual(['failed']);
    expect(viewIds(counts.waiting)).toEqual(['asks']);
  });

  it('counts nothing without a visit on record', () => {
    expect(sinceCounts([view({ status: 'failed', updated_at: at(1) })], null)).toEqual({
      merged: [],
      failed: [],
      waiting: []
    });
  });
});

describe('relativeTime', () => {
  it('reads recent times in plain words', () => {
    expect(relativeTime(at(0), NOW)).toBe('just now');
    expect(relativeTime(at(12), NOW)).toBe('12 min ago');
    expect(relativeTime(at(180), NOW)).toBe('3 h ago');
    expect(relativeTime(at(3 * 24 * 60), NOW)).toBe('3 d ago');
  });

  it('returns nothing for an unreadable time', () => {
    expect(relativeTime('not a date', NOW)).toBe('');
  });
});

describe('countText', () => {
  it('uses the singular only for one', () => {
    expect(countText(1, 'task')).toBe('1 task');
    expect(countText(2, 'task')).toBe('2 tasks');
  });
});
