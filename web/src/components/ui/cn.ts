import { clsx, type ClassValue } from 'clsx';
import { extendTailwindMerge } from 'tailwind-merge';

/**
 * The font sizes the design tokens add to Tailwind's own (`font.size` in web/design/tokens.json).
 * tailwind-merge has to be told they are sizes: otherwise it reads `text-ui` as a text colour and
 * drops it whenever a real colour class, such as `text-foreground`, comes later in the list.
 */
const TOKEN_FONT_SIZES = ['2xs', 'ui'];

const twMerge = extendTailwindMerge({ extend: { theme: { text: TOKEN_FONT_SIZES } } });

/** Join class names, resolving conflicting Tailwind utilities (the shadcn `cn`). */
export function cn(...inputs: ClassValue[]): string {
  return twMerge(clsx(inputs));
}
