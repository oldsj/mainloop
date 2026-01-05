import { request } from '@playwright/test';

const apiURL = process.env.API_URL || 'http://localhost:8081';

/**
 * Global setup - runs once before all tests.
 *
 * 1. Waits for backend API to be healthy
 * 2. Resets the database to clean state
 */
async function globalSetup() {
  const requestContext = await request.newContext();

  // Wait for API health (retry up to 30s)
  let healthy = false;
  for (let i = 0; i < 30; i++) {
    try {
      const response = await requestContext.get(`${apiURL}/health`);
      if (response.ok()) {
        healthy = true;
        break;
      }
    } catch {
      // API not ready yet
    }
    await new Promise((r) => setTimeout(r, 1000));
  }

  if (!healthy) {
    throw new Error(`API at ${apiURL} not healthy after 30s`);
  }

  // Reset database
  const resetResponse = await requestContext.post(`${apiURL}/internal/test/reset`);
  if (!resetResponse.ok()) {
    throw new Error(`Failed to reset database: ${resetResponse.status()}`);
  }

  await requestContext.dispose();
  console.log('Global setup complete: API healthy, database reset');
}

export default globalSetup;
