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
