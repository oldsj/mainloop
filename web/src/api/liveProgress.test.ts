import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  createLiveProgress,
  isProgressFrame,
  REPLY_HOLD_MS,
  type ProgressFrame
} from './liveProgress';

function frame(overrides: Partial<ProgressFrame> = {}): ProgressFrame {
  return {
    session_id: 's1',
    message_id: 'm1',
    seq: 1,
    active: true,
    started_at: '2026-10-11T12:00:00',
    status: { kind: 'thinking', label: 'Thinking…' },
    text: '',
    text_truncated: false,
    tools: [],
    ...overrides
  };
}

describe('liveProgress', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it('keeps the highest seq of a turn, and replaces it for a new message', () => {
    const live = createLiveProgress();
    live.apply(frame({ seq: 2, text: 'b' }));
    live.apply(frame({ seq: 1, text: 'a' }));
    expect(live.get('s1')?.text).toBe('b');
    live.apply(frame({ message_id: 'm2', seq: 1, text: 'next' }));
    expect(live.get('s1')?.text).toBe('next');
  });

  it('notifies subscribers on change only', () => {
    const live = createLiveProgress();
    const seen = vi.fn();
    const off = live.subscribe(seen);
    live.apply(frame());
    live.apply(frame()); // a duplicate
    expect(seen).toHaveBeenCalledTimes(1);
    off();
    live.apply(frame({ seq: 2 }));
    expect(seen).toHaveBeenCalledTimes(1);
  });

  it('holds a closed turn that names a reply, then lets it go', () => {
    const live = createLiveProgress();
    live.apply(frame({ seq: 1 }));
    live.apply(frame({ seq: 2, active: false, reply_id: 'r1' }));
    expect(live.get('s1')?.reply_id).toBe('r1');
    vi.advanceTimersByTime(REPLY_HOLD_MS + 1);
    expect(live.get('s1')).toBeUndefined();
  });

  it('keeps the held text and tools when the closing frame carries none', () => {
    const live = createLiveProgress();
    const tools = [{ id: 'c1', name: 'Read', label: 'Reading a.go', state: 'done' as const }];
    live.apply(frame({ seq: 1, text: 'so far', text_truncated: true, tools }));
    live.apply(frame({ seq: 2, active: false, reply_id: 'r1', text: '', tools: [] }));
    expect(live.get('s1')).toMatchObject({
      active: false,
      reply_id: 'r1',
      text: 'so far',
      text_truncated: true,
      tools
    });
  });

  it('shows the closing frame as it is when it carries the final state', () => {
    const live = createLiveProgress();
    live.apply(frame({ seq: 1, text: 'so far' }));
    live.apply(frame({ seq: 2, active: false, reply_id: 'r1', text: 'so far and the end' }));
    expect(live.get('s1')?.text).toBe('so far and the end');
  });

  it('does not borrow text from the frame of another message', () => {
    const live = createLiveProgress();
    live.apply(frame({ message_id: 'm1', seq: 1, text: 'old turn' }));
    live.apply(frame({ message_id: 'm2', seq: 1, active: false, reply_id: 'r2' }));
    expect(live.get('s1')?.text).toBe('');
  });

  it('forgets a closed turn without a reply at once and ignores its late frames', () => {
    const live = createLiveProgress();
    live.apply(frame({ seq: 1 }));
    live.apply(frame({ seq: 3, active: false }));
    expect(live.get('s1')).toBeUndefined();
    live.apply(frame({ seq: 2 }));
    expect(live.get('s1')).toBeUndefined();
  });

  it('forgets everything on reset', () => {
    const live = createLiveProgress();
    live.apply(frame());
    live.apply(frame({ session_id: 's2' }));
    live.reset();
    expect(live.get('s1')).toBeUndefined();
    expect(live.get('s2')).toBeUndefined();
    // A reset also lifts the block on a closed turn, so a fresh snapshot of it is shown.
    live.apply(frame({ seq: 3, active: false }));
    live.reset();
    live.apply(frame({ seq: 4 }));
    expect(live.get('s1')?.seq).toBe(4);
  });

  it('checks the fields the bubble reads', () => {
    expect(isProgressFrame(frame())).toBe(true);
    expect(isProgressFrame({ ...frame(), tools: 'no' })).toBe(false);
    expect(isProgressFrame({ ...frame(), status: null })).toBe(false);
    expect(isProgressFrame(null)).toBe(false);
    const tool = { id: 'c', name: 'Read', label: 'Reading a.go', state: 'done' };
    expect(isProgressFrame({ ...frame(), tools: [tool], reply_id: 'r' })).toBe(true);
    expect(isProgressFrame({ ...frame(), tools: [{ ...tool, state: 'lost' }] })).toBe(false);
    expect(isProgressFrame({ ...frame(), tools: [{ id: 'c' }] })).toBe(false);
    expect(isProgressFrame({ ...frame(), tools: [null] })).toBe(false);
    expect(isProgressFrame({ ...frame(), status: { kind: 'dancing', label: 'x' } })).toBe(false);
    expect(isProgressFrame({ ...frame(), status: { label: 'x' } })).toBe(false);
    expect(isProgressFrame({ ...frame(), text_truncated: undefined })).toBe(false);
    expect(isProgressFrame({ ...frame(), reply_id: 7 })).toBe(false);
  });
});
