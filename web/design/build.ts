// Generates the design-system outputs from web/design/tokens.json:
//   web/src/styles/theme.css  Tailwind v4 @theme block plus one CSS variable set per theme
//   web/design/system/        files for the published Claude Design System artifact
//
// Run with: node web/design/build.ts   (Node 24, no dependencies)
// Invalid tokens exit 1 and write nothing. Output has no timestamps and a fixed order.

import {
  existsSync,
  mkdirSync,
  readFileSync,
  readdirSync,
  rmSync,
  writeFileSync,
  copyFileSync
} from 'node:fs';
import { join } from 'node:path';

type Json = string | number | boolean | null | Json[] | { [key: string]: Json };
type JsonObject = { [key: string]: Json };

const HERE = import.meta.dirname;
const TOKENS_FILE = join(HERE, 'tokens.json');
const THEME_CSS = join(HERE, '..', 'src', 'styles', 'theme.css');
const SYSTEM_DIR = join(HERE, 'system');
const COMPONENTS_DIR = join(HERE, 'components');

// The contract with components and the artifact. Every theme must define exactly these keys.
const DEFAULT_THEME = 'eighties';
const TERM_KEYS = [
  'bg',
  'bg-secondary',
  'fg',
  'fg-muted',
  'border',
  'accent',
  'accent-alt',
  'warning',
  'error',
  'info',
  'selection',
  'cyan',
  'green',
  'yellow',
  'magenta',
  'orange',
  'purple',
  'red'
];
const SEMANTIC_KEYS = [
  'background',
  'foreground',
  'card',
  'card-foreground',
  'popover',
  'popover-foreground',
  'primary',
  'primary-foreground',
  'secondary',
  'secondary-foreground',
  'muted',
  'muted-foreground',
  'accent',
  'accent-foreground',
  'destructive',
  'destructive-foreground',
  'border',
  'input',
  'ring'
];
const REQUIRED_TOKENS = ['font.family.mono', 'radius.default'];

const TOKEN_KEYS = ['$value', '$type', '$description', '$extensions'];
const GROUP_KEYS = ['$type', '$description', '$extensions'];
const TYPES = new Set(['color', 'dimension', 'duration', 'fontFamily', 'cubicBezier']);
const GENERIC_FONTS = new Set([
  'monospace',
  'ui-monospace',
  'sans-serif',
  'ui-sans-serif',
  'serif',
  'system-ui'
]);
const ALIAS = /^\{([^{}]+)\}$/;

const errors: string[] = [];
const problem = (path: string, message: string): void => {
  errors.push(`${path}: ${message}`);
};

const isObject = (value: Json | undefined): value is JsonObject =>
  typeof value === 'object' && value !== null && !Array.isArray(value);

// Flattened view of the token tree: dotted path -> type and raw $value (aliases unresolved).
interface Token {
  type: string;
  raw: Json;
}
const tokens = new Map<string, Token>();

function collect(node: JsonObject, path: string, inheritedType: string | undefined): void {
  const ownType = node.$type;
  if (ownType !== undefined && typeof ownType !== 'string')
    problem(path || 'root', '$type must be a string');
  const type = typeof ownType === 'string' ? ownType : inheritedType;

  if ('$value' in node) {
    for (const key of Object.keys(node)) {
      if (!key.startsWith('$')) problem(path, `a token cannot have a child named ${key}`);
      else if (!TOKEN_KEYS.includes(key)) problem(path, `unknown key ${key}`);
    }
    if (!type) problem(path, 'no $type on this token or a parent group');
    else if (!TYPES.has(type)) problem(path, `unknown $type "${type}"`);
    tokens.set(path, { type: type ?? '', raw: node.$value as Json });
    return;
  }

  for (const key of Object.keys(node)) {
    if (key.startsWith('$') && !GROUP_KEYS.includes(key))
      problem(path || 'root', `unknown key ${key}`);
  }
  for (const [key, child] of Object.entries(node)) {
    if (key.startsWith('$')) continue;
    const childPath = path ? `${path}.${key}` : key;
    if (!isObject(child)) {
      problem(childPath, 'expected a group or a token');
      continue;
    }
    collect(child, childPath, type);
  }
}

const isUnit = (value: Json): boolean => typeof value === 'number' && value >= 0 && value <= 1;

// Value checks for literal (non-alias) tokens. Unknown types were already reported.
function checkValue(type: string, value: Json): string | undefined {
  switch (type) {
    case 'color':
      return typeof value === 'string' && /^#[0-9a-f]{6}$/i.test(value)
        ? undefined
        : 'expected a 6-digit hex colour such as #2d2d2d';
    case 'dimension':
      return typeof value === 'string' && /^-?\d+(\.\d+)?(px|rem|em)$/.test(value)
        ? undefined
        : 'expected a length such as 0.25rem or 0px';
    case 'duration':
      return typeof value === 'string' && /^\d+(\.\d+)?(ms|s)$/.test(value)
        ? undefined
        : 'expected a time such as 200ms or 1s';
    case 'fontFamily': {
      const names = Array.isArray(value) ? value : [value];
      const ok = names.length > 0 && names.every((name) => typeof name === 'string' && name !== '');
      return ok ? undefined : 'expected a font name or a non-empty list of font names';
    }
    case 'cubicBezier':
      return Array.isArray(value) &&
        value.length === 4 &&
        value.every((x) => typeof x === 'number') &&
        isUnit(value[0]!) &&
        isUnit(value[2]!)
        ? undefined
        : 'expected [x1, y1, x2, y2] with x1 and x2 between 0 and 1';
    default:
      return undefined;
  }
}

// Resolved values, keyed by dotted path. Aliases are replaced by their target's value.
const resolved = new Map<string, Json>();
// Tokens already reported as broken. Their dependants fail silently, so each root cause prints once.
const failed = new Set<string>();

function resolve(path: string, chain: string[]): Json | undefined {
  if (resolved.has(path)) return resolved.get(path);
  if (failed.has(path)) return undefined;
  const reject = (message: string): undefined => {
    problem(path, message);
    failed.add(path);
    return undefined;
  };
  if (chain.includes(path)) return reject(`alias cycle: ${[...chain, path].join(' -> ')}`);
  const token = tokens.get(path)!;
  let value: Json | undefined = token.raw;

  if (typeof token.raw === 'string' && token.raw.includes('{')) {
    const match = ALIAS.exec(token.raw);
    if (!match) return reject(`malformed alias ${token.raw}`);
    const target = match[1]!;
    const targetToken = tokens.get(target);
    if (!targetToken) return reject(`alias {${target}} does not resolve`);
    if (targetToken.type !== token.type)
      return reject(`alias {${target}} is ${targetToken.type}, expected ${token.type}`);
    value = resolve(target, [...chain, path]);
    if (value === undefined) {
      failed.add(path);
      return undefined;
    }
  } else {
    const message = checkValue(token.type, token.raw);
    if (message) return reject(message);
  }

  resolved.set(path, value);
  return value;
}

function checkThemes(): string[] {
  const themeNames: string[] = [];
  for (const path of tokens.keys()) {
    if (!path.startsWith('color.')) continue;
    const parts = path.split('.');
    if (
      parts.length !== 5 ||
      parts[1] !== 'theme' ||
      (parts[3] !== 'term' && parts[3] !== 'semantic')
    ) {
      problem(
        path,
        'colour tokens must be color.theme.<name>.term.<key> or color.theme.<name>.semantic.<key>'
      );
      continue;
    }
    if (!themeNames.includes(parts[2]!)) themeNames.push(parts[2]!);
  }
  if (!themeNames.includes(DEFAULT_THEME))
    problem('color.theme', `default theme "${DEFAULT_THEME}" is missing`);

  for (const name of themeNames) {
    for (const [group, expected] of [
      ['term', TERM_KEYS],
      ['semantic', SEMANTIC_KEYS]
    ] as const) {
      const prefix = `color.theme.${name}.${group}.`;
      const have = [...tokens.keys()]
        .filter((p) => p.startsWith(prefix))
        .map((p) => p.slice(prefix.length));
      for (const key of expected)
        if (!have.includes(key)) problem(prefix + key, 'missing in this theme');
      for (const key of have) if (!expected.includes(key)) problem(prefix + key, 'not a known key');
    }
  }
  return themeNames;
}

// Token path -> CSS custom property name. Unknown groups return undefined and fail the build.
function cssName(path: string): string | undefined {
  const parts = path.split('.');
  const [group, a, b] = parts;
  if (group === 'color') return parts[3] === 'term' ? `--term-${parts[4]}` : `--${parts[4]}`;
  if (group === 'font') {
    if (a === 'family') return `--font-${b}`;
    if (a === 'size') return `--text-${b}`;
    if (a === 'line-height') return `--text-${b}--line-height`;
    return undefined;
  }
  if (group === 'spacing') return `--spacing-${a}`;
  if (group === 'radius') return a === 'default' ? '--radius' : `--radius-${a}`;
  if (group === 'motion') {
    if (a === 'duration') return `--duration-${b}`;
    if (a === 'easing') return `--ease-${b}`;
    return undefined;
  }
  if (group === 'focus') return `--focus-${a}`;
  return undefined;
}

function fontStack(value: Json): string {
  const names = Array.isArray(value) ? value : [value];
  return names
    .map((name) => (GENERIC_FONTS.has(String(name)) ? String(name) : `'${String(name)}'`))
    .join(', ');
}

// CSS value for a token. An alias becomes var(--target) so a theme's semantic names follow its palette.
function cssValueOf(path: string): string {
  const token = tokens.get(path)!;
  if (typeof token.raw === 'string' && ALIAS.test(token.raw)) {
    const target = token.raw.slice(1, -1);
    return `var(${cssName(target) ?? ''})`;
  }
  const value = resolved.get(path)!;
  if (token.type === 'fontFamily') return fontStack(value);
  if (token.type === 'cubicBezier') return `cubic-bezier(${(value as number[]).join(', ')})`;
  return String(value);
}

// Source tree with every alias replaced by its resolved value. Descriptions and key order are kept.
function resolvedTree(node: JsonObject, path: string): JsonObject {
  if ('$value' in node) {
    return Object.fromEntries(
      Object.entries(node).map(([key, value]): [string, Json] => [
        key,
        key === '$value' ? resolved.get(path)! : value
      ])
    );
  }
  return Object.fromEntries(
    Object.entries(node).map(([key, value]): [string, Json] => {
      if (key.startsWith('$') || !isObject(value)) return [key, value as Json];
      return [key, resolvedTree(value, path ? `${path}.${key}` : key)];
    })
  );
}

// JSON laid out the way prettier prints it at the repo's printWidth: objects expanded, short
// arrays of primitives on one line. Keeps the generated file stable under `trunk fmt`.
const PRINT_WIDTH = 100;
function formatJson(value: Json, indent: string, prefix: string, suffix: string): string {
  if (isObject(value)) {
    const entries = Object.entries(value);
    if (entries.length === 0) return `${prefix}{}${suffix}`;
    const lines = entries.map(([key, child], index) =>
      formatJson(
        child,
        indent + '  ',
        `${indent}  ${JSON.stringify(key)}: `,
        index === entries.length - 1 ? '' : ','
      )
    );
    return `${prefix}{\n${lines.join('\n')}\n${indent}}${suffix}`;
  }
  if (Array.isArray(value)) {
    if (value.length === 0) return `${prefix}[]${suffix}`;
    if (value.every((item) => !isObject(item) && !Array.isArray(item))) {
      const inline = `[${value.map((item) => JSON.stringify(item)).join(', ')}]`;
      if (prefix.length + inline.length + suffix.length <= PRINT_WIDTH)
        return `${prefix}${inline}${suffix}`;
    }
    const lines = value.map((item, index) =>
      formatJson(item, indent + '  ', `${indent}  `, index === value.length - 1 ? '' : ',')
    );
    return `${prefix}[\n${lines.join('\n')}\n${indent}]${suffix}`;
  }
  return `${prefix}${JSON.stringify(value)}${suffix}`;
}

function checkComponents(): string[] {
  if (!existsSync(COMPONENTS_DIR)) {
    problem('components', 'directory is missing');
    return [];
  }
  const files = readdirSync(COMPONENTS_DIR)
    .filter((file) => file.endsWith('.md'))
    .sort();
  if (files.length === 0) problem('components', 'no component guideline .md files');
  return files;
}

function fail(): void {
  for (const message of errors) console.error(message);
  console.error(`${errors.length} error(s); nothing written`);
  process.exitCode = 1;
}

// ---- Load and validate -------------------------------------------------------

let source: JsonObject | undefined;
try {
  const parsed: Json = JSON.parse(readFileSync(TOKENS_FILE, 'utf8'));
  if (isObject(parsed)) source = parsed;
  else problem('root', 'expected a JSON object');
} catch (error) {
  problem('root', `not valid JSON: ${(error as Error).message}`);
}

if (source) {
  collect(source, '', undefined);
  for (const path of tokens.keys()) resolve(path, []);
  for (const path of REQUIRED_TOKENS)
    if (!tokens.has(path)) problem(path, 'required token is missing');
}
const themeNames = source ? checkThemes() : [];
const componentFiles = checkComponents();

if (errors.length > 0 || !source) {
  fail();
} else {
  // ---- Theme CSS -------------------------------------------------------------

  const themeVars = (name: string): string[] => [
    ...TERM_KEYS.map((key) => `--term-${key}: ${cssValueOf(`color.theme.${name}.term.${key}`)};`),
    ...SEMANTIC_KEYS.map((key) => `--${key}: ${cssValueOf(`color.theme.${name}.semantic.${key}`)};`)
  ];

  // Non-colour tokens: Tailwind namespaces go in @theme, the rest are plain variables on :root.
  const themeNamespaced: string[] = [];
  const sharedRoot: string[] = [];
  for (const path of tokens.keys()) {
    if (path.startsWith('color.')) continue;
    const name = cssName(path);
    if (!name) {
      problem(path, 'no CSS mapping for this group');
      continue;
    }
    const line = `${name}: ${cssValueOf(path)};`;
    if (/^--(font|text|spacing|radius|ease)-/.test(name)) themeNamespaced.push(line);
    else sharedRoot.push(line);
  }
  if (errors.length > 0) {
    fail();
  } else {
    const indent = (lines: string[]): string[] => lines.map((line) => `  ${line}`);
    const colourMap = [
      ...TERM_KEYS.map((key) => `--color-term-${key}: var(--term-${key});`),
      ...SEMANTIC_KEYS.map((key) => `--color-${key}: var(--${key});`)
    ];

    const css: string[] = [
      '/* Generated by web/design/build.ts from web/design/tokens.json. Do not edit by hand. */',
      '',
      '@theme {',
      '  /* Colour utilities read these variables at runtime, so theme switching works. */',
      ...indent(colourMap),
      '',
      ...indent(themeNamespaced),
      '}',
      '',
      ':root {',
      `  /* ${DEFAULT_THEME} is the default theme. */`,
      ...indent(themeVars(DEFAULT_THEME)),
      '',
      ...indent(sharedRoot),
      '}',
      ''
    ];
    for (const name of themeNames) {
      if (name === DEFAULT_THEME) continue;
      css.push(`[data-theme='${name}'] {`, ...indent(themeVars(name)), '}', '');
    }

    // ---- Design system folder ------------------------------------------------

    const guidelinesDir = join(SYSTEM_DIR, 'guidelines');
    const systemReadme = [
      '# Mainloop design system',
      '',
      'Generated by `web/design/build.ts` from `web/design/tokens.json`. Do not edit this folder by hand.',
      '',
      'Mainloop is a terminal-style interface: monospace type, square corners, and five dark colour themes.',
      '',
      '## Tokens',
      '',
      '- `tokens.json`: resolved values in W3C Design Tokens format. Aliases are already resolved.',
      `- Themes: ${themeNames.join(', ')}. \`${DEFAULT_THEME}\` is the default.`,
      '- Colours: `term-*` is the terminal palette (background, text, accent, status colours). Semantic names such as `background`, `primary`, `muted` and `border` follow shadcn/ui and map onto that palette.',
      '- Type: JetBrains Mono, with sizes in `font.size` and line heights in `font.line-height`.',
      '- Radius is 0 everywhere: no rounded corners, pills or circular avatars.',
      '',
      '## Components',
      '',
      "Each file in `guidelines/` covers one product component: purpose, states, keyboard and screen-reader behaviour, touch targets, and do and don't.",
      '',
      ...componentFiles.map((file) => `- \`guidelines/${file}\``),
      '',
      '## Previews',
      '',
      'Run `pnpm --dir web build:design` from the repository root to build `previews/index.html` and one page per `*.preview.tsx`. Serve this folder over HTTP to view the interactive fixtures.',
      'Previews are built in CI, not committed: hashed bundles would add churn without providing extra drift evidence. Theme and guideline outputs remain committed and drift-checked.',
      'Run `pnpm --dir web check:design` after building to check every page with axe in Chromium. Serious and critical violations fail; lower impacts are reported.',
      '',
      '## Using it in Claude Design',
      '',
      '- Use this system as the default for Mainloop designs.',
      '- Build from the tokens and components here. Do not add new colours, radii, font sizes or spacing values to a design; ask for a token change instead.',
      '- Token changes go to `tokens.json` in the repository first, never straight into a design file.',
      ''
    ].join('\n');

    rmSync(SYSTEM_DIR, { recursive: true, force: true });
    mkdirSync(guidelinesDir, { recursive: true });
    for (const file of componentFiles)
      copyFileSync(join(COMPONENTS_DIR, file), join(guidelinesDir, file));
    writeFileSync(join(SYSTEM_DIR, 'README.md'), systemReadme);
    writeFileSync(
      join(SYSTEM_DIR, 'tokens.json'),
      formatJson(resolvedTree(source, ''), '', '', '') + '\n'
    );

    mkdirSync(join(THEME_CSS, '..'), { recursive: true });
    writeFileSync(THEME_CSS, css.join('\n').replace(/\n+$/, '\n'));

    console.log(
      `wrote web/src/styles/theme.css (${themeNames.length} themes) and web/design/system/ (${componentFiles.length} guidelines)`
    );
  }
}
