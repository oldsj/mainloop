import { readFileSync, statSync } from 'node:fs';
import { describe, expect, it } from 'vitest';

// The type contract: the page is a sans, and the generated theme carries both families and the two
// chrome sizes. The transcript and code opt into the monospace with font-mono.
// A function, not a literal `new URL('…', import.meta.url)`: the bundler rewrites that form for assets.
const at = (path) => new URL(path, import.meta.url);
const read = (path) => readFileSync(at(path), 'utf8');
const theme = read('../src/styles/theme.css');
const base = read('../src/styles/index.css');
const { family, size } = JSON.parse(read('./tokens.json')).font;

describe('type', () => {
  it('defines a sans for the chrome and a monospace for the transcript and code', () => {
    expect(family.sans.$value[0]).toBe('system-ui');
    expect(family.sans.$value.at(-1)).toBe('sans-serif');
    expect(family.mono.$value.at(-1)).toBe('monospace');
    expect(theme).toMatch(/--font-sans: system-ui, [^;]*sans-serif;/);
    // SF Mono where the browser has it; otherwise Geist Mono, which the app serves itself.
    expect(theme).toMatch(
      /--font-mono:\s+ui-monospace, 'SF Mono', 'SFMono-Regular', 'Geist Mono', 'Menlo', monospace;/
    );
    expect(theme).not.toMatch(/JetBrains/);
  });

  it('serves the fallback monospace itself: one small file, its licence beside it, no font service', () => {
    // Given the face the stack names for browsers with no SF Mono to give
    const face = /@font-face \{[^}]*\}/.exec(base)?.[0] ?? '';
    expect(face).toContain("font-family: 'Geist Mono';");
    expect(face).toContain('font-display: swap;');
    expect(face).toContain("url('../assets/fonts/GeistMono-Variable.woff2') format('woff2')");
    // One variable file carries every weight, so it is declared for the whole range.
    expect(face).toContain('font-weight: 100 900;');

    // Then the file is in the repository, is a woff2, and stays inside its budget (80 KB)
    const file = at('../src/assets/fonts/GeistMono-Variable.woff2');
    expect(readFileSync(file).subarray(0, 4).toString('latin1')).toBe('wOF2');
    expect(statSync(file).size).toBeLessThan(80 * 1024);

    // And it is preloaded from the page, with its licence shipped beside the app
    const page = read('../index.html');
    expect(page).toMatch(
      /<link\s+rel="preload"\s+href="\/src\/assets\/fonts\/GeistMono-Variable\.woff2"\s+as="font"\s+type="font\/woff2"\s+crossorigin\s*\/>/
    );
    expect(page).not.toMatch(/fonts\.g(oogleapis|static)/);
    expect(read('../public/licenses/GeistMono-OFL.txt')).toContain(
      'SIL OPEN FONT LICENSE Version 1.1'
    );
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
