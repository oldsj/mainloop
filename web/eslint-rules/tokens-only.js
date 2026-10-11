// Components take colours and corner radius from design tokens only (web/design/README.md).
// Bans, in string literals and template text:
//   - colour literals (#hex, rgb(), hsl(), oklch(), ...);
//   - Tailwind palette colours (bg-red-500, text-white, ...);
//   - every radius class except rounded-none. Bare `rounded` and `rounded-full` are hard-coded
//     in Tailwind v4 and do not read the --radius-* tokens, so they must not appear at all.

const COLOUR_LITERAL = /#[0-9a-fA-F]{3,8}\b|\b(?:rgba?|hsla?|hwb|lab|lch|oklab|oklch|color)\(/;
const PALETTE =
  /^-?(?:bg|text|border|ring|fill|stroke|from|via|to|outline|divide|accent|caret|decoration|shadow)-(?:(?:slate|gray|zinc|neutral|stone|red|orange|amber|yellow|lime|green|emerald|teal|cyan|sky|blue|indigo|violet|purple|fuchsia|pink|rose)-\d{2,3}|black|white)(?:\/\d+)?$/;
const RADIUS = /^rounded(?:-(?:t|r|b|l|s|e|tl|tr|br|bl|ss|se|ee|es))?(?:-(.+))?$/;

/** Class tokens in a string, with any variant prefix (`md:hover:`) and `!` removed. */
export function classTokens(text) {
  return text
    .split(/\s+/)
    .filter(Boolean)
    .map((token) => token.replace(/^(?:[^\s:[\]]+:|\[[^\]]*\]:)+/, '').replace(/^!/, ''));
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
