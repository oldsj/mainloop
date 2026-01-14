import { sveltekit } from '@sveltejs/kit/vite';
import tailwindcss from '@tailwindcss/vite';
import { defineConfig, type Plugin } from 'vite';

// Force full page reload when store files change to prevent stale state
function storeHmrPlugin(): Plugin {
  return {
    name: 'store-hmr',
    handleHotUpdate({ file, server }) {
      if (file.includes('/stores/') && file.endsWith('.ts')) {
        server.ws.send({ type: 'full-reload' });
        return [];
      }
    }
  };
}

export default defineConfig({
  plugins: [sveltekit(), tailwindcss(), storeHmrPlugin()]
});
