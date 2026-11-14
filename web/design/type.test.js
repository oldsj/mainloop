import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';

// The type contract: the page is a sans, and the generated theme carries both families and the two
// chrome sizes. The transcript and code opt into the monospace with font-mono.
const read = (path) => readFileSync(new URL(path, import.meta.url), 'utf8');
const theme = read('../src/styles/theme.css');
const base = read('../src/styles/index.css');
const { family, size } = JSON.parse(read('./tokens.json')).font;

describe('type', () => {
  it('defines a sans for the chrome and a monospace for the transcript and code', () => {
    expect(family.sans.$value[0]).toBe('system-ui');
    expect(family.sans.$value.at(-1)).toBe('sans-serif');
    expect(family.mono.$value.at(-1)).toBe('monospace');
    expect(theme).toMatch(/--font-sans: system-ui, [^;]*sans-serif;/);
    expect(theme).toMatch(/--font-mono: [^;]*monospace;/);
  });

  it('makes the sans the page default', () => {
    const body = /body \{[^}]*\}/.exec(base)?.[0] ?? '';
    expect(body).toContain('font-family: var(--font-sans);');
    expect(body).not.toContain('--font-mono');
  });

  it('adds the 11px and 13px chrome sizes', () => {
    expect(size['2xs'].$value).toBe('0.6875rem');
    expect(size.ui.$value).toBe('0.8125rem');
    expect(theme).toContain('--text-2xs: 0.6875rem;');
    expect(theme).toContain('--text-ui: 0.8125rem;');
  });
});
