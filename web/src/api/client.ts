import createFetchClient from 'openapi-fetch';
import createClient from 'openapi-react-query';
import { API_URL } from './config';
import { connection } from './connection';
import { createMiddleware } from './middleware';
import type { paths } from './schema.gen';

/**
 * The typed fetch client. Components use `$api`; non-React code (probes, tests) can use this.
 *
 * `fetch` is looked up on every call, not captured when this module loads: a test server (MSW)
 * patches `globalThis.fetch` after imports run, and a captured reference would bypass it.
 */
export const fetchClient = createFetchClient<paths>({
  baseUrl: API_URL,
  fetch: (request) => globalThis.fetch(request)
});
fetchClient.use(createMiddleware({ reportFailure: () => connection.reportFailure() }));

/** React Query bindings over the same client: `$api.useQuery('get', '/attention')`. */
export const $api = createClient(fetchClient);
