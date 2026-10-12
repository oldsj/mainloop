// Components take colours and corner radius from design tokens only (web/design/README.md).
// Bans, in string literals and template text:
//   - colour literals (#hex, rgb(), hsl(), oklch(), ...);
//   - Tailwind palette colours (bg-red-500, text-white, ...);
//   - every radius class except rounded-none. Bare `rounded` and `rounded-full` are hard-coded
//     in Tailwind v4 and do not read the --radius-* tokens, so they must not appear at all.

import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';

const COLOUR_LITERAL = /#[0-9a-fA-F]{3,8}\b|\b(?:rgba?|hsla?|hwb|lab|lch|oklab|oklch|color)\(/;

// Palette names come from Tailwind's own theme, so a palette added by a Tailwind upgrade (4.3 has
// mauve, olive, mist and taupe) is caught without editing this file.
function paletteNames() {
  const theme = createRequire(import.meta.url).resolve('tailwindcss/theme.css');
  const names = new Set(
    [...readFileSync(theme, 'utf8').matchAll(/--color-([a-z]+)-50:/g)].map((m) => m[1])
  );
  if (!names.size)
    throw new Error('tokens-only: no palette colours found in tailwindcss/theme.css');
  return [...names];
}

// Utilities that take a colour. Longer names come first so `ring-offset` is not read as `ring`.
const COLOUR_UTILITIES =
  '(?:bg|text-shadow|text|border(?:-[xytrblse]{1,2})?|ring-offset|ring|inset-ring|inset-shadow|' +
  'drop-shadow|shadow|fill|stroke|from|via|to|outline|divide|accent|caret|decoration|placeholder)';
const PALETTE = new RegExp(
  `^-?${COLOUR_UTILITIES}-(?:(?:${paletteNames().join('|')})-\\d{2,3}|black|white)(?:/.+)?$`
);
const RADIUS = /^rounded(?:-(?:t|r|b|l|s|e|tl|tr|br|bl|ss|se|ee|es))?(?:-(.+))?$/;

/**
 * The utility part of a class token: everything after the last variant, however the variants are
 * written (`md:`, `data-[state=open]:`, `has-[>svg]:`, `[&:hover]:`), with the `!` important
 * marker removed from either end. A colon inside brackets belongs to the variant.
 */
export function utilityOf(token) {
  let depth = 0;
  let cut = 0;
  for (let i = 0; i < token.length; i++) {
    const ch = token[i];
    if (ch === '[' || ch === '(') depth++;
    else if (ch === ']' || ch === ')') depth = Math.max(0, depth - 1);
    else if (ch === ':' && depth === 0) cut = i + 1;
  }
  return token.slice(cut).replace(/^!/, '').replace(/!$/, '');
}

/** Class tokens in a string, each reduced to its utility. */
export function classTokens(text) {
  return text.split(/\s+/).filter(Boolean).map(utilityOf);
}

export function problemsIn(text) {
  const found = [];
  if (COLOUR_LITERAL.test(text)) found.push('colour literal: use a design token');
  for (const token of classTokens(text)) {
    if (PALETTE.test(token)) found.push(`palette colour "${token}": use a token utility`);
    const radius = RADIUS.exec(token);
    if (radius && radius[1] !== 'none') {
      // `rounded-t-none` parses as side "t" + "none"; `rounded-lg` has no side.
      found.push(`radius class "${token}": corners are square, use rounded-none`);
    }
  }
  return found;
}

const rule = {
  meta: { type: 'problem', schema: [], messages: { bad: '{{ detail }}' } },
  create(context) {
    const check = (node, text) => {
      for (const detail of problemsIn(text))
        context.report({ node, messageId: 'bad', data: { detail } });
    };
    return {
      Literal(node) {
        if (typeof node.value === 'string') check(node, node.value);
      },
      TemplateElement(node) {
        check(node, node.value.raw);
      }
    };
  }
};

export default { rules: { 'tokens-only': rule } };
