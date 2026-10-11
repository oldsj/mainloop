import { describe, expect, it } from 'vitest';
import {
  ageLabel,
  attentionItems,
  badgeText,
  prLink,
  taskIdOf,
  unresolvedCount,
  withoutQueueItem
} from './model';
import { attentionItem, attentionSnapshot, queueItem } from './test/fixtures';

describe('attention boundary', () => {
  it('drops malformed rows rather than showing them half-formed', () => {
    const good = attentionItem(queueItem());
    const snapshot = attentionSnapshot([
      good,
      { ...good, id: 'x', kind: 'mystery' },
      { ...good, id: 'y', queue_item: 'not-an-object' },
      { id: 'z' }
    ] as never);
    expect(attentionItems(snapshot).map((item) => item.id)).toEqual([good.id]);
  });

  it('treats a missing or negative count as zero and floors fractions', () => {
    expect(unresolvedCount(undefined)).toBe(0);
    expect(unresolvedCount(attentionSnapshot([], -4))).toBe(0);
    expect(unresolvedCount(attentionSnapshot([], Number.NaN))).toBe(0);
    expect(unresolvedCount(attentionSnapshot([], 3.7))).toBe(3);
  });

  it('removes one queue row and reduces the count without going below zero', () => {
    const a = attentionItem(queueItem({ id: 'a' }));
    const b = attentionItem(queueItem({ id: 'b' }));
    const next = withoutQueueItem(attentionSnapshot([a, b], 2), 'a');
    expect(next.items.map((item) => item.id)).toEqual([b.id]);
    expect(next.count).toBe(1);
    expect(withoutQueueItem(attentionSnapshot([a], 0), 'a').count).toBe(0);
  });
});

describe('badge text', () => {
  it('shows nothing at zero, the number, and 99+ above that', () => {
    expect(badgeText(0)).toBeNull();
    expect(badgeText(2)).toBe('2');
    expect(badgeText(99)).toBe('99');
    expect(badgeText(100)).toBe('99+');
  });
});

describe('task links', () => {
  it('prefers the task id and falls back to a /tasks link', () => {
    expect(taskIdOf('task-1', '/tasks/other')).toBe('task-1');
    expect(taskIdOf(null, '/tasks/task%201')).toBe('task 1');
    expect(taskIdOf(null, '/?inbox=abc')).toBeNull();
    expect(taskIdOf(null, '/tasks/a/b')).toBeNull();
  });
});

describe('PR links', () => {
  it('offers only https URLs', () => {
    const pr = (url: unknown) => prLink(queueItem({ context: { pr_url: url } }));
    expect(pr('https://github.com/owner/repo/pull/7')).toBe('https://github.com/owner/repo/pull/7');
    expect(pr('javascript:alert(1)')).toBeNull();
    expect(pr('http://example.com')).toBeNull();
    expect(pr(42)).toBeNull();
  });
});

describe('age label', () => {
  const now = Date.parse('2026-10-11T12:00:00.000Z');
  it('uses the compact units the inbox has always shown', () => {
    expect(ageLabel('2026-10-11T11:59:40.000Z', now)).toBe('now');
    expect(ageLabel('2026-10-11T11:55:00.000Z', now)).toBe('5m');
    expect(ageLabel('2026-10-11T09:00:00.000Z', now)).toBe('3h');
    expect(ageLabel('2026-10-09T12:00:00.000Z', now)).toBe('2d');
    expect(ageLabel('not a date', now)).toBe('');
  });
});
