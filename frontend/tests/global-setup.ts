import { request } from '@playwright/test';

const apiURL = process.env.API_URL || 'http://localhost:8000';

/**
 * Global setup - runs once before all tests.
 *
 * Lesson #3 & #5: Fail fast with proper health checks.
 * If the API isn't healthy quickly, something is wrong - fail immediately.
 *
 * 1. Waits for backend API to be healthy (30s max, 1s intervals)
 * 2. Resets the database to clean state
 */
async function globalSetup() {
  console.log('=== Global Setup Starting ===');
  console.log(`API URL: ${apiURL}`);

  const requestContext = await request.newContext();

  // Wait for API health - fail fast if not ready
  console.log('Waiting for API health...');
  await waitForHealthy(requestContext, apiURL, { maxRetries: 30, retryDelay: 1000 });

  // Reset database to clean state
  console.log('Resetting database...');
  const resetResponse = await requestContext.post(`${apiURL}/internal/test/reset`);
  if (!resetResponse.ok()) {
    throw new Error(`Failed to reset database: ${resetResponse.status()}`);
  }

  await requestContext.dispose();
  console.log('=== Global Setup Complete: API healthy, database reset ===');
}

/**
 * Wait for API to be healthy. Fail fast if deployment is broken.
 */
async function waitForHealthy(
  requestContext: Awaited<ReturnType<typeof request.newContext>>,
  url: string,
  options: { maxRetries: number; retryDelay: number }
): Promise<void> {
  for (let i = 0; i < options.maxRetries; i++) {
    try {
      const response = await requestContext.get(`${url}/health`);
      if (response.ok()) {
        console.log(`API healthy after ${i + 1} attempt(s)`);
        return;
      }
      console.log(`API returned ${response.status()}, retrying...`);
    } catch (error) {
      if (i === 0) {
        console.log('API not responding, retrying...');
      }
    }

    if (i < options.maxRetries - 1) {
      await new Promise((r) => setTimeout(r, options.retryDelay));
    }
  }

  throw new Error(
    `API at ${url} not healthy after ${options.maxRetries}s. Check deployment:\n` +
      `  kubectl logs -n mainloop deployment/mainloop-backend --tail=50`
  );
}

export default globalSetup;
