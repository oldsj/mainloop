import { describe, expect, it } from 'vitest';
import { memoryOrigin } from '@/features/projects/model';
import { memoryEntry } from '@/features/projects/test/fixtures';
import {
  confirmedAge,
  isDue,
  ownerMemoryLists,
  ownerMemoryOrigin,
  shortCommit
} from './ownerMemory';
import { ownerEntry } from './test/ownerMemory';

const NOW = Date.parse('2026-11-27T12:00:00Z');

describe('owner memory model', () => {
  it('is due once the review window has run out, and only while active', () => {
    expect(isDue(ownerEntry({ due_at: '2026-11-27T12:00:00Z' }), NOW)).toBe(true);
    expect(isDue(ownerEntry({ due_at: '2026-11-28T00:00:00Z' }), NOW)).toBe(false);
    expect(isDue(ownerEntry({ state: 'lapsed', due_at: '2026-11-01T00:00:00Z' }), NOW)).toBe(false);
    expect(isDue(ownerEntry({ due_at: null }), NOW)).toBe(false);
  });

  it('lists due and lapsed entries oldest confirmation first, and due ones as active too', () => {
    const due = ownerEntry({
      id: 'due',
      last_confirmed_at: '2026-11-05T00:00:00Z',
      due_at: '2026-11-19T00:00:00Z'
    });
    const lapsed = ownerEntry({
      id: 'lapsed',
      state: 'lapsed',
      last_confirmed_at: '2026-11-01T00:00:00Z',
      lapsed_at: '2026-11-22T00:00:00Z'
    });
    const fresh = ownerEntry({ id: 'fresh', due_at: '2026-12-10T00:00:00Z' });
    const proposed = ownerEntry({ id: 'p', state: 'proposed', last_confirmed_at: null });
    const lists = ownerMemoryLists([fresh, due, lapsed, proposed, { id: 'broken' }], NOW);
    expect(lists.proposed.map((e) => e.id)).toEqual(['p']);
    expect(lists.review.map((e) => e.id)).toEqual(['lapsed', 'due']);
    expect(lists.active.map((e) => e.id)).toEqual(['fresh', 'due']);
  });

  it('says how long ago an entry was confirmed', () => {
    const at = (iso: string) => ownerEntry({ last_confirmed_at: iso });
    expect(confirmedAge(at('2026-11-27T01:00:00Z'), NOW)).toBe('confirmed today');
    expect(confirmedAge(at('2026-11-26T10:00:00Z'), NOW)).toBe('confirmed 1 day ago');
    expect(confirmedAge(at('2026-11-15T12:00:00Z'), NOW)).toBe('confirmed 12 days ago');
    expect(confirmedAge(ownerEntry({ last_confirmed_at: null }), NOW)).toBeNull();
  });

  it("names main's entries and a task's proposals", () => {
    const ago = () => '2d ago';
    expect(ownerMemoryOrigin(ownerEntry(), ago)).toBe('By main, 2d ago');
    expect(ownerMemoryOrigin(ownerEntry({ author: 'task' }), ago)).toBe(
      'Proposed by a task, 2d ago'
    );
  });

  it('shortens a commit', () => {
    expect(shortCommit('0123456789abcdef')).toBe('0123456');
  });

  it('says a project entry moved from owner memory came from there', () => {
    const moved = memoryEntry({
      source: {
        session_id: null,
        task_id: null,
        attempt_id: null,
        delivery_id: null,
        owner_entry_id: 'om-1'
      }
    });
    expect(memoryOrigin(moved, () => '1d ago')).toBe('Moved from owner memory, 1d ago');
  });
});
