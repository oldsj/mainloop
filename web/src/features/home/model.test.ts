import { describe, expect, it } from 'vitest';
import type { TaskView } from '@/lib/contract';
import { NOW, projection, view } from '@/test/fixtures/tasks';
import { countText, isIdle, sinceCounts, statusLine, workGroup } from './model';

const MIN = 60_000;
const at = (minutesBeforeNow: number) => new Date(NOW - minutesBeforeNow * MIN).toISOString();
const LAST_VISIT = NOW - 60 * MIN; // the owner was last here an hour ago

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

  it('puts a waiting/publication task with an open, green, unmerged PR in Needs you, never Working', () => {
    const stalled = view(
      { id: 'd', status: 'waiting', reason: 'publication' },
      { projection: projection({ pr_state: 'open', ci_state: 'success' }) }
    );
    expect(workGroup(stalled, LAST_VISIT)).toBe('needs-you');
  });

  it('keeps a waiting/publication task in Working while CI has not gone green', () => {
    const ciPending = view(
      { id: 'e', status: 'waiting', reason: 'publication' },
      { projection: projection({ pr_state: 'open', ci_state: 'pending' }) }
    );
    expect(workGroup(ciPending, LAST_VISIT)).toBe('working');
  });

  it('puts failed and blocked tasks in Blocked or failed', () => {
    expect(workGroup(view({ status: 'failed' }), LAST_VISIT, NOW)).toBe('blocked');
    expect(workGroup(view({ status: 'blocked' }), LAST_VISIT, NOW)).toBe('blocked');
  });

  it('gives a failed task that asks nothing no group once it is over seven days old', () => {
    // Given a failed task that can still be retried.
    const DAY = 24 * 60 * MIN;
    const failedAt = (daysAgo: number) =>
      view({ status: 'failed', updated_at: new Date(NOW - daysAgo * DAY).toISOString() });

    // Then it counts for seven days, and not after.
    expect(workGroup(failedAt(7), LAST_VISIT, NOW)).toBe('blocked');
    expect(workGroup(failedAt(8), LAST_VISIT, NOW)).toBeNull();
    // A blocked task is still alive, however long it has waited.
    const blocked = view({ status: 'blocked', updated_at: new Date(NOW - 30 * DAY).toISOString() });
    expect(workGroup(blocked, LAST_VISIT, NOW)).toBe('blocked');
  });

  it('keeps an old failed task that asks the owner something in Needs you', () => {
    const old = new Date(NOW - 30 * 24 * 60 * MIN).toISOString();
    const asking = view({ status: 'failed', reason: 'owner_decision', updated_at: old });
    expect(workGroup(asking, LAST_VISIT, NOW)).toBe('needs-you');
  });

  it('gives a failed task with nothing left to do no group at all', () => {
    const stuck = view(
      { status: 'failed' },
      {
        actions: {
          retry: { available: false, reason: 'handoff_unavailable' },
          reassign: { available: false, reason: 'handoff_unavailable' },
          cancel: { available: false, reason: 'cancel_unavailable' }
        }
      }
    );
    expect(workGroup(stuck, LAST_VISIT)).toBeNull();
  });

  it('puts a blocked task stuck on Mainloop’s own recovery work in Problems, not Needs you', () => {
    const reasons = ['session_lost', 'stalled', 'runtime_limit', 'reconciliation'] as const;
    for (const reason of reasons) {
      expect(workGroup(view({ status: 'blocked', reason }), LAST_VISIT)).toBe('problem');
    }
    // A real owner decision stays a decision, never a problem.
    expect(workGroup(view({ status: 'blocked', reason: 'owner_decision' }), LAST_VISIT)).toBe(
      'needs-you'
    );
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

describe('a standing supervisor at rest', () => {
  const idle = view({ standing: true, mode: 'coordination', status: 'waiting', reason: 'idle' });

  it('is in no group: it is waiting, but not on anything', () => {
    expect(isIdle(idle)).toBe(true);
    expect(workGroup(idle, null)).toBeNull();
    // Waiting on something else is still work in progress.
    expect(isIdle(view({ status: 'waiting', reason: 'ci' }))).toBe(false);
    expect(workGroup(view({ status: 'waiting', reason: 'ci' }), null)).toBe('working');
  });

  it('reads Idle, not Waiting', () => {
    expect(statusLine(idle)).toBe('Idle');
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

describe('countText', () => {
  it('uses the singular only for one', () => {
    expect(countText(1, 'task')).toBe('1 task');
    expect(countText(2, 'task')).toBe('2 tasks');
  });
});
