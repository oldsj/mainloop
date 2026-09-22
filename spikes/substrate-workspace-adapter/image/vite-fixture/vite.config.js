import { defineConfig } from 'vite';

export default defineConfig({
  server: {
    host: '0.0.0.0',
    port: 80,
    strictPort: true,
    allowedHosts: true,
    // No hmr.clientPort override: the browser reaches this actor through a proxy whose port
    // varies by deployment (port-forward, ingress, ...). Vite infers the HMR client's port from
    // window.location by default, which is correct for same-origin proxying (our NGINX
    // header-proxy) and was the actual bug the first time this was set to the actor's internal
    // port 80 -- the browser tried to open a WebSocket to its own port 80, not the proxy's port.
    // Measured live: the actual variable was NOT inotify-vs-polling (the default inotify watch
    // picks up an atomic rename-replace write, e.g. `sed -i`, correctly, with polling enabled or
    // not). It was the write method -- a plain shell-redirect truncate-in-place write (`cmd >
    // file`) was never observed by Vite's watcher on this gVisor-sandboxed filesystem, with or
    // without polling, while an atomic rename-replace write (`sed -i`, or any editor/tool that
    // writes-then-renames, which is how most real editors and Node's own atomic-write helpers
    // behave) was picked up every time and produced a true HMR update, not a reload. usePolling
    // is left enabled here only as defense in depth; it was not the fix.
    watch: { usePolling: true, interval: 300 }
  }
});
