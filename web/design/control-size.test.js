import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';
import { DESKTOP_QUERY } from '../src/shell/view.ts';

// The control size contract, read from the token source and the generated theme: touch-sized by
// default, compact only in the desktop layout with a mouse or trackpad. Components reach it
// through min-h-control and w-control.
const read = (path) => readFileSync(new URL(path, import.meta.url), 'utf8');
const css = read('../src/styles/theme.css');
const { touch, pointer } = JSON.parse(read('./tokens.json')).size.control;

describe('control size', () => {
  it('is 44px by default and 28px for a fine pointer', () => {
    expect(touch.$value).toBe('2.75rem');
    expect(pointer.$value).toBe('1.75rem');
  });

  it('declares the touch size in @theme, so min-h-control and w-control exist', () => {
    const theme = /@theme \{[^}]*\}/.exec(css)?.[0] ?? '';
    expect(theme).toContain(`--spacing-control: ${touch.$value};`);
  });

  it('applies the pointer size only in the desktop layout with a fine pointer', () => {
    const values = [...css.matchAll(/--spacing-control: ([^;]+);/g)].map((m) => m[1]);
    expect(values).toEqual([touch.$value, pointer.$value]);
    const media = /@media ([^{]+) \{\s*:root \{\s*--spacing-control: ([^;]+);/.exec(css);
    expect(media?.[1]).toBe(`${DESKTOP_QUERY} and (pointer: fine)`);
    expect(media?.[2]).toBe(pointer.$value);
  });
});
