import { describe, expect, it } from 'vitest';
import { LiveModel } from './store';

const sysLines = (m: LiveModel) => m.state.entries.flatMap((e) => (e.t === 'sys' ? [e.text] : []));

describe('model and effort acks', () => {
  it('prints a change from the last ack only, and an empty value as the default', () => {
    const m = new LiveModel();
    m.hello({ k: 'hello', v: 1, meta: {}, backlog: [], runner: { state: 'ready' } });
    // The SDK driver sets both at the start of every turn; the first acks are the baseline.
    m.apply({ k: 'ctl', what: 'model', model: 'sonnet' });
    m.apply({ k: 'ctl', what: 'effort', effort: '' });
    // The SDK's init names the full model id, which is shown but is not an ack.
    m.apply({ k: 'sdk', m: { type: 'system', subtype: 'init', model: 'claude-sonnet-5-5' } });
    expect(m.state.model).toBe('claude-sonnet-5-5');
    m.apply({ k: 'ctl', what: 'model', model: 'sonnet' });
    m.apply({ k: 'ctl', what: 'effort', effort: '' });
    expect(sysLines(m)).toEqual([]);
    // Another client changed them.
    m.apply({ k: 'ctl', what: 'model', model: 'opus' });
    m.apply({ k: 'ctl', what: 'effort', effort: 'high' });
    m.apply({ k: 'ctl', what: 'model', model: 'opus' });
    m.apply({ k: 'ctl', what: 'effort', effort: 'high' });
    expect(sysLines(m)).toEqual(['Set model to opus', 'Set effort to high']);
    m.apply({ k: 'ctl', what: 'effort', effort: '' });
    expect(sysLines(m).at(-1)).toBe('Set effort to default');
    expect(m.state.effort).toBe('');
    expect(m.state.model).toBe('opus');
  });

  it('does not print the ack of a change this view made through Mainloop', () => {
    const m = new LiveModel();
    m.hello({ k: 'hello', v: 1, meta: {}, backlog: [], runner: { state: 'ready' } });
    m.apply({ k: 'ctl', what: 'model', model: 'sonnet' });
    m.control('model', 'opus');
    m.apply({ k: 'ctl', what: 'model', model: 'opus' });
    m.control('effort', '');
    expect(sysLines(m)).toEqual(['Set model to opus', 'Set effort to default']);
  });
});

describe('history', () => {
  it('places tool calls from the record as the live view shows them, edits as diffs', () => {
    const m = new LiveModel();
    m.setHistory([
      { role: 'user', content: 'fix it' },
      {
        role: 'tool',
        name: 'Bash',
        input: { command: 'go test ./...' },
        result: '\nFAIL x\nok y',
        error: true
      },
      {
        role: 'tool',
        name: 'Edit',
        input: { file_path: '/data/workspace/a.go', old_string: 'a', new_string: 'b' },
        result: 'ok',
        error: false
      },
      { role: 'tool', name: 'Read', input: 'cut {"file_pa', result: null, error: false },
      { role: 'assistant', content: 'Fixed.' }
    ]);
    const kinds = m.state.entries.map((e) => e.t);
    expect(kinds).toEqual(['sys', 'user', 'tool', 'tool', 'diff', 'tool', 'text']);
    const tools = m.state.entries.filter((e) => e.t === 'tool');
    expect(tools[0]).toMatchObject({
      name: 'Bash',
      arg: 'go test ./...',
      out: ['FAIL x', 'ok y'],
      status: 'err'
    });
    expect(tools[1]).toMatchObject({ name: 'Edit', arg: 'a.go', status: 'ok' });
    // A call whose arguments were cut, with no result stored.
    expect(tools[2]).toMatchObject({ name: 'Read', arg: 'cut {"file_pa', out: [], status: 'ok' });
    expect(m.state.entries[4]).toMatchObject({ t: 'diff', verb: 'Update', file: 'a.go' });
  });
});
