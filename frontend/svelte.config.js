import adapterNode from '@sveltejs/adapter-node';
import adapterCloudflare from '@sveltejs/adapter-cloudflare';
import { vitePreprocess } from '@sveltejs/vite-plugin-svelte';

// Use Node adapter for Docker dev, Cloudflare for production
const adapter = process.env.USE_NODE_ADAPTER === 'true' ? adapterNode() : adapterCloudflare();

/** @type {import('@sveltejs/kit').Config} */
const config = {
  preprocess: vitePreprocess(),
  kit: {
    adapter
  }
};

export default config;
