import { describe, expect, it } from 'vitest';
import { view } from '@/test/fixtures/tasks';
import { needsTotal, ownerWork, problemTotal } from './ownerWork';
import { attentionItem, problemAttentionItem, queueItem } from './test/fixtures';

const NOW = Date.parse('2026-01-01T12:00:00Z');

describe('owner work', () => {
  it('lists the owner actions, then the tasks that need a decision and that no item names', () => {
    const question = attentionItem(queueItem({ id: 'q1' }));
    const failed = view({ id: 'failed', status: 'failed' });
    const work = ownerWork([question], [failed], NOW);
    expect(work.needs).toEqual([question]);
    expect(work.needsTasks.map((task) => task.task.id)).toEqual(['failed']);
    expect(needsTotal(work)).toBe(2);
    expect(problemTotal(work)).toBe(0);
  });

  it('does not list a task twice when an item names it, by its task id or by its link', () => {
    const byId = view({ id: 'by-id', status: 'blocked', reason: 'owner_decision' });
    const byLink = view({ id: 'by-link', status: 'blocked', reason: 'owner_decision' });
    const items = [
      attentionItem(queueItem({ id: 'a' }), { task_id: 'by-id' }),
      attentionItem(queueItem({ id: 'b' }), { task_id: null, href: '/tasks/by-link' })
    ];
    const work = ownerWork(items, [byId, byLink], NOW);
    expect(work.needsTasks).toEqual([]);
    expect(needsTotal(work)).toBe(2);
  });

  it('keeps problems apart: a problem item, and a task Mainloop is recovering', () => {
    const stalled = view({ id: 'stalled', status: 'blocked', reason: 'stalled' });
    const named = view({ id: 'task-1', status: 'blocked', reason: 'session_lost' });
    const work = ownerWork([problemAttentionItem()], [stalled, named], NOW);
    expect(work.needs).toEqual([]);
    expect(work.needsTasks).toEqual([]);
    expect(work.problems).toHaveLength(1);
    expect(work.problemTasks.map((task) => task.task.id)).toEqual(['stalled']);
    expect(problemTotal(work)).toBe(2);
    expect(needsTotal(work)).toBe(0);
  });

  it('leaves out a task the main thread has set aside, until its time passes', () => {
    const later = new Date(NOW + 60_000).toISOString();
    const held = view({ id: 'held', status: 'failed', held_until: later });
    expect(needsTotal(ownerWork([], [held], NOW))).toBe(0);
    expect(needsTotal(ownerWork([], [held], NOW + 120_000))).toBe(1);
  });

  it('leaves out a failure nobody retried once it is over seven days old', () => {
    // Given two failed tasks that can still be retried, one from yesterday and one from last month.
    const DAY = 24 * 60 * 60 * 1000;
    const recent = view({
      id: 'recent',
      status: 'failed',
      updated_at: new Date(NOW - DAY).toISOString()
    });
    const stale = view({
      id: 'stale',
      status: 'failed',
      updated_at: new Date(NOW - 33 * DAY).toISOString()
    });

    // When the owner's work is listed.
    const work = ownerWork([], [stale, recent], NOW);

    // Then only yesterday's failure needs them.
    expect(work.needsTasks.map((task) => task.task.id)).toEqual(['recent']);
    expect(needsTotal(work)).toBe(1);
  });

  it('leaves out work that is running, finished, or failed with nothing left to do', () => {
    const none = { available: false, reason: 'cancel_unavailable' } as const;
    const tasks = [
      view({ id: 'running', status: 'running' }),
      view({ id: 'done', status: 'completed' }),
      view(
        { id: 'dead', status: 'failed' },
        { actions: { retry: none, reassign: none, cancel: none } }
      )
    ];
    const work = ownerWork([], tasks, NOW);
    expect(needsTotal(work)).toBe(0);
    expect(problemTotal(work)).toBe(0);
  });

  it('puts the newest task first', () => {
    const older = view({ id: 'older', status: 'failed', updated_at: '2025-12-30T00:00:00Z' });
    const newer = view({ id: 'newer', status: 'failed', updated_at: '2025-12-31T00:00:00Z' });
    expect(ownerWork([], [older, newer], NOW).needsTasks.map((task) => task.task.id)).toEqual([
      'newer',
      'older'
    ]);
  });
});
