/// <reference types="vitest/config" />
import { tanstackRouter } from '@tanstack/router-plugin/vite';
import tailwindcss from '@tailwindcss/vite';
import react from '@vitejs/plugin-react';
import { fileURLToPath } from 'node:url';
import { defineConfig } from 'vite';

// The local Go `serve-api` listens here; override with MAINLOOP_API_PROXY.
const apiProxy = process.env.MAINLOOP_API_PROXY ?? 'http://127.0.0.1:8000';

export default defineConfig({
  plugins: [
    // semicolons and quoteStyle match what the file already holds, so the plugin's output is stable
    tanstackRouter({
      target: 'react',
      autoCodeSplitting: true,
      semicolons: true,
      quoteStyle: 'single'
    }),
    react(),
    tailwindcss()
  ],
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) }
  },
  server: {
    host: process.env.VITE_HOST ?? '127.0.0.1',
    port: Number(process.env.VITE_PORT ?? 5173),
    proxy: {
      // VITE_API_URL defaults to /api, which the dev server forwards with the prefix stripped.
      '/api': {
        target: apiProxy,
        changeOrigin: true,
        rewrite: (path) => path.replace(/^\/api/, '')
      }
    }
  },
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: ['./src/test/setup.ts'],
    include: ['src/**/*.test.{ts,tsx}', 'eslint-rules/*.test.js'],
    css: false,
    // Absolute, so requests resolve to a real URL under jsdom and MSW can match them.
    env: { VITE_API_URL: 'http://localhost:3000/api' }
  }
});
