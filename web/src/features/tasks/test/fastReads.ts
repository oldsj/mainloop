import type * as Middleware from '@/api/middleware';

/**
 * The app's middleware with reads that skip the rate limiter. The page makes several reads at once,
 * and past the limiter's small burst they queue for a quarter-second each, so an action's re-read
 * takes seconds under test. A test file uses this in its own `vi.mock('@/api/middleware', ...)`:
 * vitest hoists mocks per file, so the mock cannot live in a shared helper.
 */
export function withFastReads(actual: typeof Middleware): typeof Middleware {
  return {
    ...actual,
    createMiddleware: (deps) =>
      actual.createMiddleware({
        ...deps,
        acquireRead: deps.acquireRead ?? (() => Promise.resolve())
      })
  };
}
