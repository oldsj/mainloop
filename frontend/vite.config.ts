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

// Dev only: with VITE_API_URL=/api the browser talks to this server, which forwards to the
// backend. A remote dev box then needs one forwarded port (the UI) instead of two, and the
// backend's localhost-only CORS rule never comes into play.
const apiProxyTarget = process.env.MAINLOOP_API_PROXY || 'http://localhost:8000';

export default defineConfig({
  plugins: [sveltekit(), tailwindcss(), storeHmrPlugin()],
  server: {
    proxy: {
      '/api': {
        target: apiProxyTarget,
        changeOrigin: true,
        rewrite: (path) => path.replace(/^\/api/, '')
      }
    }
  }
});
