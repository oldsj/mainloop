import { readdirSync, readFileSync, statSync } from 'node:fs';
import { describe, expect, it } from 'vitest';

// The type contract: the chrome and prose are the system sans, and code is the monospace. The
// generated theme carries exactly the tokens' two families, five size roles (plus the field size)
// and three weights, with Tailwind's own scale cleared, and no component reaches outside them.
// A function, not a literal `new URL('…', import.meta.url)`: the bundler rewrites that form for assets.
const at = (path) => new URL(path, import.meta.url);
const read = (path) => readFileSync(at(path), 'utf8');
const theme = read('../src/styles/theme.css');
const base = read('../src/styles/index.css');
const { family, size, weight } = JSON.parse(read('./tokens.json')).font;
const keys = (group) => Object.keys(group).filter((key) => !key.startsWith('$'));

/** Every source file under src/, with its text. */
function sources() {
  const src = at('../src/');
  return readdirSync(src, { recursive: true })
    .filter((name) => /\.(tsx?|css)$/.test(String(name)) && !String(name).endsWith('.gen.ts'))
    .map((name) => ({
      name: String(name),
      text: readFileSync(new URL(String(name), src), 'utf8')
    }));
}

describe('type', () => {
  it('defines two families: the system sans for the chrome and a monospace for code', () => {
    expect(keys(family)).toEqual(['sans', 'mono']);
    expect(family.sans.$value[0]).toBe('system-ui');
    expect(family.sans.$value.at(-1)).toBe('sans-serif');
    expect(family.mono.$value.at(-1)).toBe('monospace');
    expect(theme).toMatch(/--font-sans: system-ui, [^;]*sans-serif;/);
    // Tailwind's own families, sizes and weights are cleared, so only the tokens exist.
    expect(theme).toContain('--font-*: initial;');
    expect(theme).toContain('--text-*: initial;');
    expect(theme).toContain('--font-weight-*: initial;');
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

  it('makes the sans, at the body size, the page default', () => {
    const body = /body \{[^}]*\}/.exec(base)?.[0] ?? '';
    expect(body).toContain('font-family: var(--font-sans);');
    expect(body).toContain('font-size: var(--text-body);');
    expect(body).not.toContain('--font-mono');
  });

  it('has five size roles and a field size, 13px body with a pointer and 15px on touch', () => {
    expect(keys(size)).toEqual(['label', 'caption', 'body', 'field', 'title', 'display']);
    expect(size.label.$value).toBe('0.6875rem');
    expect(size.caption.$value).toBe('0.75rem');
    expect(size.body.pointer.$value).toBe('0.8125rem');
    expect(size.body.touch.$value).toBe('0.9375rem');
    // A phone zooms in on a field under 16px.
    expect(size.field.touch.$value).toBe('1rem');
    expect(theme).toContain('--text-body: 0.9375rem;');
    expect(theme).toMatch(
      /@media[^{]*pointer: fine[^{]*\{\s*:root \{[^}]*--text-body: 0\.8125rem;/
    );
  });

  it('names no size after a colour, or its utility would set both', () => {
    const colours = new Set([...theme.matchAll(/--color-([a-z-]+):/g)].map((m) => m[1]));
    expect(keys(size).filter((name) => colours.has(name))).toEqual([]);
  });

  it('has three weights', () => {
    expect(keys(weight)).toEqual(['normal', 'medium', 'semibold']);
    expect(theme).toContain('--font-weight-semibold: 600;');
  });

  it('keeps components on the tokens: no stray family, size or weight', () => {
    const files = sources();
    expect(files.length).toBeGreaterThan(0);
    const sizes = keys(size).join('|');
    const weights = keys(weight).join('|');
    const rules = [
      // The sans is the page's own; mono is the only family a component names.
      [/(?<![\w-])font-(sans|serif)\b/, 'font-sans or font-serif'],
      // Sizes are the role tokens; Tailwind's scale is cleared, so its names would do nothing.
      [
        new RegExp(`(?<![\\w-])text-(?!(${sizes})\\b)(xs|sm|base|lg|[2-9]?xl|2xs|ui)\\b`),
        'a size outside the tokens'
      ],
      [/(?<![\w-])text-\[\d/, 'an arbitrary text size'],
      [
        new RegExp(
          `(?<![\\w-])font-(?!(${weights}|mono)\\b)(thin|extralight|light|bold|extrabold|black)\\b`
        ),
        'a weight outside the tokens'
      ]
    ];
    const offenders = files.flatMap(({ name, text }) =>
      rules.filter(([rule]) => rule.test(text)).map(([, what]) => `${name}: ${what}`)
    );
    expect(offenders).toEqual([]);
  });
});
