import { describe, expect, it } from 'vitest';
import tokens from '../../design/tokens.json';

// Every theme's colours against WCAG AA, read from web/design/tokens.json so a token edit that
// breaks a pair fails here. Text needs 4.5:1; borders and other non-text UI need 3:1 against bg.
//
// Selected and hovered rows and the active tab fill with `accent` and put `accent-foreground` text
// on it. Muted and status text on that fill is checked where it passes. Where it fails, the pair
// is listed in NOT_CHECKED_ON_SELECTION with its reason, and a test asserts that it really fails.

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

// Hue in degrees (0 to 360) of a hex colour, and the shorter arc between two hues.
function hueOf(hex: string): number {
  const [r, g, b] = [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16) / 255) as [
    number,
    number,
    number
  ];
  const max = Math.max(r, g, b);
  const span = max - Math.min(r, g, b);
  if (span === 0) return 0;
  const sector =
    max === r ? ((g - b) / span) % 6 : max === g ? (b - r) / span + 2 : (r - g) / span + 4;
  return (sector * 60 + 360) % 360;
}

function hueDistance(a: string, b: string): number {
  const apart = Math.abs(hueOf(a) - hueOf(b));
  return Math.min(apart, 360 - apart);
}

const AA_TEXT = 4.5;
const AA_NON_TEXT = 3;

// Status colours that carry meaning as text on the page background and on bg-secondary.
const STATUS = [
  'accent-alt',
  'warning',
  'info',
  'cyan',
  'green',
  'yellow',
  'magenta',
  'orange',
  'purple'
];

// Text colours checked on the selection fill: muted text, then each status colour.
const ON_SELECTION = [
  'fg-muted',
  'accent',
  'accent-alt',
  'warning',
  'error',
  'info',
  'cyan',
  'green',
  'yellow',
  'magenta',
  'orange',
  'purple'
];

// Pairs on the selection fill that are not checked, by theme. Each one is below 4.5:1 today, and
// a test asserts that, so an entry cannot hide a pass.
//
// Reason, for every entry: no component draws these colours as text on a selected or hovered
// fill. Those rows and the active tab use accent-foreground text, and a status there is carried by
// its label or glyph (see web/design/components/session-summary.md and status-badge.md). Muted
// text is held to bg and bg-secondary only, so it is excluded from the fill the same way.
const NOT_CHECKED_ON_SELECTION: Record<string, string[]> = {
  eighties: [
    'fg-muted',
    'accent',
    'accent-alt',
    'warning',
    'error',
    'info',
    'cyan',
    'green',
    'magenta',
    'orange',
    'purple'
  ],
  dracula: [],
  nord: [
    'fg-muted',
    'accent',
    'accent-alt',
    'error',
    'info',
    'cyan',
    'green',
    'magenta',
    'orange',
    'purple'
  ],
  gruvbox: [
    'fg-muted',
    'accent-alt',
    'warning',
    'error',
    'info',
    'cyan',
    'green',
    'magenta',
    'orange',
    'purple'
  ],
  'tokyo-night': ['fg-muted', 'accent', 'error', 'magenta', 'orange', 'purple']
};

type Check = [label: string, foreground: string, background: string, minimum: number];

// [label, foreground, background, minimum ratio] for one theme.
function checks(theme: string): Check[] {
  const term = (key: string) => colour(theme, 'term', key);
  const semantic = (key: string) => colour(theme, 'semantic', key);
  const excluded = NOT_CHECKED_ON_SELECTION[theme] ?? [];
  const statusOnBackgrounds: Check[] = STATUS.flatMap((key): Check[] => [
    [`${key} text on bg`, term(key), term('bg'), AA_TEXT],
    [`${key} text on bg-secondary`, term(key), term('bg-secondary'), AA_TEXT]
  ]);
  const onSelection: Check[] = ON_SELECTION.filter((key) => !excluded.includes(key)).map(
    (key): Check => [`${key} text on selection`, term(key), semantic('accent'), AA_TEXT]
  );
  return [
    ['text on bg', term('fg'), term('bg'), AA_TEXT],
    ['text on bg-secondary', term('fg'), term('bg-secondary'), AA_TEXT],
    // The selected row and the active tab: accent-foreground text on the accent fill.
    ['selected text on accent fill', semantic('accent-foreground'), semantic('accent'), AA_TEXT],
    ...onSelection,
    ['muted text on bg', term('fg-muted'), term('bg'), AA_TEXT],
    ['muted text on bg-secondary', term('fg-muted'), term('bg-secondary'), AA_TEXT],
    ['accent text on bg', term('accent'), term('bg'), AA_TEXT],
    ['accent text on bg-secondary', term('accent'), term('bg-secondary'), AA_TEXT],
    ['error text on bg', term('error'), term('bg'), AA_TEXT],
    ['error text on bg-secondary', term('error'), term('bg-secondary'), AA_TEXT],
    ...statusOnBackgrounds,
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

describe('pairs not checked on the selection fill', () => {
  const excluded = Object.entries(NOT_CHECKED_ON_SELECTION).flatMap(([theme, keys]) =>
    keys.map((key): [string, string] => [theme, key])
  );

  it.each(excluded)(
    '%s: %s is below 4.5:1 on selection, so the exclusion is needed',
    (theme, key) => {
      expect(
        contrast(colour(theme, 'term', key), colour(theme, 'semantic', 'accent'))
      ).toBeLessThan(AA_TEXT);
    }
  );
});

describe('muted text hierarchy', () => {
  // Body text stays a clear step brighter than muted text, so the two read as different levels.
  it.each(Object.keys(themes))('%s: body text is at least 1.4x muted text', (theme) => {
    expect(
      contrast(colour(theme, 'term', 'fg'), colour(theme, 'term', 'fg-muted')),
      `fg vs fg-muted in ${theme}`
    ).toBeGreaterThanOrEqual(1.4);
  });
});

describe('Dracula accent and purple', () => {
  // Both are light enough for text on bg-secondary, so hue is what tells them apart.
  it('stays at least 20 degrees apart in hue', () => {
    expect(
      hueDistance(colour('dracula', 'term', 'accent'), colour('dracula', 'term', 'purple'))
    ).toBeGreaterThanOrEqual(20);
  });
});
