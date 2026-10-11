import { describe, expect, it } from 'vitest';
import tokens from '../../design/tokens.json';

// Every theme's colours against WCAG AA, read from web/design/tokens.json so a token edit that
// breaks a pair fails here. Text needs 4.5:1; borders and other non-text UI need 3:1 against bg.
//
// Not checked: term-selection against bg. Muted text sits on the selection fill (SessionRow's
// active row). A 3:1 selection against a dark bg would need that muted text to be nearly white,
// so text wins and the selection fill keeps its value.

type Group = 'term' | 'semantic';
type Colours = Record<Group, Record<string, { $value: string }>>;
const themes = tokens.color.theme as Record<string, Colours>;

// Follows an alias such as {color.theme.eighties.term.bg} to its hex value.
function colour(theme: string, group: Group, key: string): string {
  const value = themes[theme]![group][key]!.$value;
  const alias = /^\{color\.theme\.([^.]+)\.(term|semantic)\.([^}]+)\}$/.exec(value);
  return alias ? colour(alias[1]!, alias[2] as Group, alias[3]!) : value;
}

// WCAG 2.x relative luminance and contrast ratio.
function luminance(hex: string): number {
  const [r, g, b] = [1, 3, 5].map((i) => {
    const channel = parseInt(hex.slice(i, i + 2), 16) / 255;
    return channel <= 0.03928 ? channel / 12.92 : ((channel + 0.055) / 1.055) ** 2.4;
  }) as [number, number, number];
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}

function contrast(a: string, b: string): number {
  const [light, dark] = [luminance(a), luminance(b)].sort((x, y) => y - x) as [number, number];
  return (light + 0.05) / (dark + 0.05);
}

const AA_TEXT = 4.5;
const AA_NON_TEXT = 3;

// [label, foreground, background, minimum ratio] for one theme.
function checks(theme: string): [string, string, string, number][] {
  const term = (key: string) => colour(theme, 'term', key);
  const semantic = (key: string) => colour(theme, 'semantic', key);
  return [
    ['text on bg', term('fg'), term('bg'), AA_TEXT],
    ['text on bg-secondary', term('fg'), term('bg-secondary'), AA_TEXT],
    ['text on selection', term('fg'), term('selection'), AA_TEXT],
    ['muted text on bg', term('fg-muted'), term('bg'), AA_TEXT],
    ['muted text on bg-secondary', term('fg-muted'), term('bg-secondary'), AA_TEXT],
    ['muted text on selection', term('fg-muted'), term('selection'), AA_TEXT],
    ['accent text on bg', term('accent'), term('bg'), AA_TEXT],
    ['accent text on bg-secondary', term('accent'), term('bg-secondary'), AA_TEXT],
    ['error text on bg', term('error'), term('bg'), AA_TEXT],
    ['error text on bg-secondary', term('error'), term('bg-secondary'), AA_TEXT],
    ['magenta text on bg', term('magenta'), term('bg'), AA_TEXT],
    ['orange text on bg', term('orange'), term('bg'), AA_TEXT],
    ['purple text on bg', term('purple'), term('bg'), AA_TEXT],
    [
      'destructive button text',
      semantic('destructive-foreground'),
      semantic('destructive'),
      AA_TEXT
    ],
    ['primary button text', semantic('primary-foreground'), semantic('primary'), AA_TEXT],
    ['border against bg', term('border'), term('bg'), AA_NON_TEXT]
  ];
}

describe('theme colour contrast (WCAG AA)', () => {
  it('uses the WCAG formula', () => {
    expect(contrast('#000000', '#ffffff')).toBeCloseTo(21, 5);
  });

  const cases = Object.keys(themes).flatMap((theme) =>
    checks(theme).map(
      ([label, foreground, background, minimum]): [string, string, string, string, number] => [
        theme,
        label,
        foreground,
        background,
        minimum
      ]
    )
  );

  it.each(cases)('%s: %s', (theme, label, foreground, background, minimum) => {
    expect(contrast(foreground, background), `${label} in ${theme}`).toBeGreaterThanOrEqual(
      minimum
    );
  });
});
