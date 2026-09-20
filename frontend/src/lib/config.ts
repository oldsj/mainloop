/** Base URL of the Mainloop backend. In dev, `/api` is proxied by Vite (see vite.config.ts). */
export const API_URL = import.meta.env.VITE_API_URL || 'http://localhost:8000';
