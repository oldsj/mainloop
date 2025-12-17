import type { Config } from 'tailwindcss';
import { theme } from './theme';

export default {
  theme: {
    extend: {
      colors: theme.colors,
      spacing: theme.spacing,
      fontSize: theme.fontSize,
      borderRadius: theme.borderRadius
    }
  }
} satisfies Config;
