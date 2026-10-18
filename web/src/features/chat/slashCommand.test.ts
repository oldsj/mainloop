import { describe, expect, it } from 'vitest';
import {
  activeSlashToken,
  matchSlashCommands,
  parseSlashCommand,
  type SlashCommandSpec
} from './slashCommand';

const REGISTRY: SlashCommandSpec[] = [
  { name: 'model', summary: 'Switch the model' },
  { name: 'mode', summary: 'Switch the mode' }
];

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

describe('matchSlashCommands', () => {
  it('matches a prefix case-insensitively, in registry order', () => {
    expect(matchSlashCommands(REGISTRY, 'Mo')).toEqual(REGISTRY);
    expect(matchSlashCommands(REGISTRY, 'model')).toEqual([REGISTRY[0]]);
  });

  it('lists every command for an empty query', () => {
    expect(matchSlashCommands(REGISTRY, '')).toEqual(REGISTRY);
  });

  it('is empty when nothing matches', () => {
    expect(matchSlashCommands(REGISTRY, 'zzz')).toEqual([]);
  });
});

describe('activeSlashToken', () => {
  it('is null for text that does not start with a slash', () => {
    expect(activeSlashToken('hello', 0)).toBeNull();
    expect(activeSlashToken('  /model', 2)).toBeNull();
  });

  it('is null when the caret sits before the slash', () => {
    expect(activeSlashToken('/model', 0)).toBeNull();
  });

  it('reads the query typed so far while the caret is inside the name token', () => {
    expect(activeSlashToken('/', 1)).toEqual({ query: '', start: 0, end: 1 });
    expect(activeSlashToken('/mo', 3)).toEqual({ query: 'mo', start: 0, end: 3 });
    // The caret can sit anywhere inside the token, not only at its end.
    expect(activeSlashToken('/model', 3)).toEqual({ query: 'model', start: 0, end: 6 });
  });

  it('is null once the caret has moved past the name into the arguments', () => {
    expect(activeSlashToken('/model codex', 7)).toBeNull();
    expect(activeSlashToken('/model codex', 12)).toBeNull();
  });
});
