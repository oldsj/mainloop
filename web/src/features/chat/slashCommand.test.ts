import { describe, expect, it } from 'vitest';
import { parseSlashCommand } from './slashCommand';

describe('parseSlashCommand', () => {
  it('is null for ordinary text, including text that merely mentions a slash', () => {
    expect(parseSlashCommand('hello')).toBeNull();
    expect(parseSlashCommand('a/b')).toBeNull();
    expect(parseSlashCommand('  not a command')).toBeNull();
  });

  it('is null for a bare slash with nothing after it', () => {
    expect(parseSlashCommand('/')).toBeNull();
    expect(parseSlashCommand('/   ')).toBeNull();
  });

  it('reads the command name lowercased, with no arguments', () => {
    expect(parseSlashCommand('/Model')).toEqual({ name: 'model', args: [] });
  });

  it('splits arguments on whitespace, collapsing runs and trimming ends', () => {
    expect(parseSlashCommand('/model   codex   medium  ')).toEqual({
      name: 'model',
      args: ['codex', 'medium']
    });
  });

  it('leaves the command name as typed; callers decide case-insensitive matching for args', () => {
    expect(parseSlashCommand('/model Codex')).toEqual({ name: 'model', args: ['Codex'] });
  });
});
