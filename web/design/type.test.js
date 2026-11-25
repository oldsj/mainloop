import { readdirSync, readFileSync, statSync } from 'node:fs';
import { describe, expect, it } from 'vitest';

// The type contract: the whole app is one monospace, so the chrome matches the transcript. The
// generated theme carries that one family and the two chrome sizes, and no component picks another.
// A function, not a literal `new URL('…', import.meta.url)`: the bundler rewrites that form for assets.
const at = (path) => new URL(path, import.meta.url);
const read = (path) => readFileSync(at(path), 'utf8');
const theme = read('../src/styles/theme.css');
const base = read('../src/styles/index.css');
const { family, size } = JSON.parse(read('./tokens.json')).font;

describe('type', () => {
  it('defines one family, a monospace', () => {
    expect(Object.keys(family)).toEqual(['mono']);
    expect(family.mono.$value.at(-1)).toBe('monospace');
    expect(theme).not.toMatch(/--font-(sans|serif):/);
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

  it('makes the monospace the page default', () => {
    const body = /body \{[^}]*\}/.exec(base)?.[0] ?? '';
    expect(body).toContain('font-family: var(--font-mono);');
    expect(body).not.toContain('--font-sans');
  });

  it('leaves the family to the page: no component sets one', () => {
    // Given every source file under src/
    const src = at('../src/');
    const files = readdirSync(src, { recursive: true }).filter((name) =>
      /\.(tsx?|css)$/.test(String(name))
    );
    expect(files.length).toBeGreaterThan(0);

    // Then none picks a family with a utility, so nothing escapes the one monospace
    const offenders = files.filter((name) =>
      /(?<![\w-])font-(sans|serif|mono)\b/.test(readFileSync(new URL(String(name), src), 'utf8'))
    );
    expect(offenders).toEqual([]);
  });

  it('adds the 11px and 13px chrome sizes', () => {
    expect(size['2xs'].$value).toBe('0.6875rem');
    expect(size.ui.$value).toBe('0.8125rem');
    expect(theme).toContain('--text-2xs: 0.6875rem;');
    expect(theme).toContain('--text-ui: 0.8125rem;');
  });
});
