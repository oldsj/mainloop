import type { Config } from 'tailwindcss';
import tailwindPreset from '@mainloop/ui/tailwind.preset';

export default {
  presets: [tailwindPreset],
  content: ['./src/**/*.{html,js,svelte,ts}']
} satisfies Config;
