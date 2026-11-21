import { http, HttpResponse } from 'msw';
import { API_URL } from '@/api/config';
import { connection } from '@/api/connection';
import { server } from './msw/server';

/**
 * Makes the app's connection store see the backend as unreachable, the way it finds out for real:
 * `/health` gets no answer, and one probe runs. Only that probe fails, so a test file that probes
 * again afterwards (`connection.check()`) is back online. The store is a module singleton: without
 * such a probe it stays offline for the rest of the file.
 */
export async function backendUnreachable(): Promise<void> {
  server.use(http.get(`${API_URL}/health`, () => HttpResponse.error(), { once: true }));
  await connection.check();
}
