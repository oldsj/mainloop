import { describe, expect, it } from 'vitest';
import {
  sessionSearch,
  toHistory,
  toolArg,
  transcriptItems,
  transcriptSearch,
  type TranscriptEntry
} from './model';

let pos = 0;
const entry = (
  id: number | null,
  kind: TranscriptEntry['kind'],
  text: string,
  more: Partial<TranscriptEntry> = {}
): TranscriptEntry => ({
  id,
  task_id: 't1',
  pos: pos++,
  kind,
  tool_name: '',
  tool_call_id: '',
  is_error: false,
  text,
  truncated: false,
  orig_bytes: text.length,
  at: null,
  live: id === null,
  ...more
});

describe('transcriptItems', () => {
  it('pairs a call with its result by turn and call id, across pages', () => {
    const items = transcriptItems([
      entry(1, 'user', 'go'),
      entry(2, 'tool_call', '{"command":"ls"}', { tool_name: 'Bash', tool_call_id: 'c1' }),
      entry(3, 'tool_call', '{"file_path":"a"}', { tool_name: 'Read', tool_call_id: 'c2' }),
      entry(4, 'tool_result', 'a', { tool_name: 'Read', tool_call_id: 'c2' }),
      // Another turn reusing a call id does not pair with the first turn's call.
      entry(5, 'tool_result', 'x', { tool_name: 'Bash', tool_call_id: 'c1', task_id: 't2' }),
      entry(6, 'tool_result', 'ls out', { tool_name: 'Bash', tool_call_id: 'c1' })
    ]);
    expect(
      items.map((i) => (i.t === 'tool' ? [i.call?.id ?? null, i.result?.id ?? null] : i.entry.id))
    ).toEqual([1, [2, 6], [3, 4], [null, 5]]);
  });

  it('keys a live entry, which has no id, by its turn and position', () => {
    const [item] = transcriptItems([entry(null, 'assistant', 'hi', { pos: 3 })]);
    expect(item?.key).toBe('live-t1-3');
  });
});

describe('toolArg', () => {
  it('reads the argument the live view shows, or the text when the arguments were cut', () => {
    expect(toolArg('Bash', '{"command":"go test ./..."}')).toBe('go test ./...');
    expect(toolArg('Read', '{"file_path":"/data/workspace/web/a.ts"}')).toBe('web/a.ts');
    expect(toolArg('Write', '{"file_path":"/data/wor')).toBe('{"file_path":"/data/wor');
  });
});

describe('toHistory', () => {
  it('keeps messages and tool calls, and leaves out thinking and status', () => {
    expect(
      toHistory([
        entry(1, 'user', 'go'),
        entry(2, 'thinking', 'hmm'),
        entry(3, 'tool_call', '{"command":"ls"}', { tool_name: 'Bash', tool_call_id: 'c1' }),
        entry(4, 'tool_result', 'boom', { tool_name: 'Bash', tool_call_id: 'c1', is_error: true }),
        entry(5, 'status', 'working'),
        entry(6, 'assistant', 'done')
      ])
    ).toEqual([
      { role: 'user', content: 'go' },
      { role: 'tool', name: 'Bash', input: { command: 'ls' }, result: 'boom', error: true },
      { role: 'assistant', content: 'done' }
    ]);
  });
});

describe('URL schemas', () => {
  it('reads the session view, dropping what it does not know', () => {
    expect(sessionSearch.parse({ transcript: 1, entry: '42' })).toEqual({
      transcript: 1,
      entry: 42
    });
    expect(sessionSearch.parse({ transcript: 'yes', entry: -1 })).toEqual({
      transcript: undefined,
      entry: undefined
    });
  });

  it('keeps a query of digits, which the URL parser reads as a number', () => {
    expect(transcriptSearch.parse({ q: 448 })).toEqual({ q: '448' });
    expect(transcriptSearch.parse({})).toEqual({});
  });
});
