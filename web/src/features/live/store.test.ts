import { describe, expect, it } from 'vitest';
import { LiveModel } from './store';

const sysLines = (m: LiveModel) => m.state.entries.flatMap((e) => (e.t === 'sys' ? [e.text] : []));

describe('model and effort acks', () => {
  it('prints a change only, and an empty value as the default', () => {
    const m = new LiveModel();
    m.hello({
      k: 'hello',
      v: 1,
      meta: { model: 'sonnet' },
      backlog: [],
      runner: { state: 'ready' }
    });
    // The SDK driver sets both at the start of every turn.
    m.apply({ k: 'ctl', what: 'model', model: 'sonnet' });
    m.apply({ k: 'ctl', what: 'effort', effort: '' });
    expect(sysLines(m)).toEqual([]);
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
});
