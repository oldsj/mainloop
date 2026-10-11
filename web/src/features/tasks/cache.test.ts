import { describe, expect, it } from 'vitest';
import { attempt, projection, view } from '@/test/fixtures/tasks';
import { isTaskView, mergeTaskCache, mergeTaskListCache, tasksNeedingOwner } from './cache';

describe('isTaskView', () => {
  it('accepts a complete view', () => {
    expect(isTaskView(view())).toBe(true);
  });

  it.each([
    ['null', null],
    ['an array', []],
    ['no task', { attempts: [] }],
    ['a task without an id', { task: { ...view().task, id: undefined } }],
    ['a task with a numeric title', { task: { ...view().task, title: 7 } }],
    ['a task whose version is a string', { task: { ...view().task, version: '3' } }],
    ['a task without created_at', { task: { ...view().task, created_at: undefined } }],
    ['attempts that are not a list', { ...view(), attempts: {} }],
    ['a projection that is a string', { ...view(), projection: 'observed' }],
    ['actions that are null', { ...view(), actions: null }]
  ])('rejects %s', (_label, value) => {
    expect(isTaskView(value)).toBe(false);
  });
});

describe('mergeTaskCache', () => {
  it('takes the first view when none is held', () => {
    const next = view({ version: 4 });
    expect(mergeTaskCache(undefined, next)).toMatchObject({ task: { version: 4 } });
  });

  it('keeps the held view when a late read is older', () => {
    const held = view({ version: 5, title: 'Newer' });
    const late = view({ version: 4, title: 'Older' });
    expect(mergeTaskCache(held, late)).toBe(held);
  });

  it('takes a newer view', () => {
    const held = view({ version: 5, title: 'Old title' });
    const next = view({ version: 6, title: 'New title' });
    expect(mergeTaskCache(held, next)).toMatchObject({ task: { version: 6, title: 'New title' } });
  });

  it('keeps the held reference when an equal view arrives', () => {
    const held = view({ version: 5 });
    const same = view({ version: 5 });
    expect(mergeTaskCache(held, same)).toBe(held);
  });

  it('passes a malformed payload through so the page can report it', () => {
    const held = view({ version: 5 });
    const broken = { task: { id: 'task-1' } };
    expect(mergeTaskCache(held, broken)).toBe(broken);
  });
});

describe('mergeTaskListCache', () => {
  it('takes the rows of a fresh list', () => {
    const rows = [view({ id: 'a' }), view({ id: 'b' })];
    expect(mergeTaskListCache(undefined, rows)).toEqual(rows);
  });

  it('takes membership from the latest read, so a task that left the list leaves it', () => {
    const held = [view({ id: 'a' }), view({ id: 'b' })];
    const next = [view({ id: 'a' })];
    const merged = mergeTaskListCache(held, next) as unknown[];
    expect(merged).toHaveLength(1);
    expect(merged[0]).toMatchObject({ task: { id: 'a' } });
  });

  it('keeps a held row that is newer than the row just read', () => {
    const heldRow = view({ id: 'a', version: 7 });
    const merged = mergeTaskListCache([heldRow], [view({ id: 'a', version: 6 })]) as unknown[];
    expect(merged[0]).toBe(heldRow);
  });

  it('drops invalid rows rather than failing the list', () => {
    const merged = mergeTaskListCache(undefined, [
      view({ id: 'a' }),
      { task: { id: 'broken' } },
      null
    ]) as unknown[];
    expect(merged).toHaveLength(1);
    expect(merged[0]).toMatchObject({ task: { id: 'a' } });
  });

  it('keeps the previous list reference when nothing changed', () => {
    const held = [view({ id: 'a', version: 2 })];
    expect(mergeTaskListCache(held, [view({ id: 'a', version: 2 })])).toBe(held);
  });

  it('passes a non-list payload through', () => {
    const body = { detail: 'unexpected' };
    expect(mergeTaskListCache([], body)).toBe(body);
  });
});

describe('tasksNeedingOwner', () => {
  it('includes a task with a pending approval', () => {
    const waiting = view(
      { id: 'approve' },
      {
        projection: projection({ pending_approval_ids: ['approval-1'] })
      }
    );
    expect(tasksNeedingOwner([waiting]).map((v) => v.task.id)).toEqual(['approve']);
  });

  it('includes a task whose reason needs an owner decision', () => {
    const decide = view({ id: 'decide', reason: 'owner_decision' });
    expect(tasksNeedingOwner([decide]).map((v) => v.task.id)).toEqual(['decide']);
  });

  it('excludes a task that is simply running', () => {
    const running = view({ id: 'running', reason: null }, { attempts: [attempt()] });
    expect(tasksNeedingOwner([running])).toEqual([]);
  });
});
