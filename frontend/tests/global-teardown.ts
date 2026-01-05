import { request } from '@playwright/test';

const apiURL = process.env.API_URL || 'http://localhost:8000';

/**
 * Global teardown - runs once after all tests complete.
 */
async function globalTeardown() {
  try {
    const requestContext = await request.newContext();
    await requestContext.post(`${apiURL}/internal/test/reset`);
    await requestContext.dispose();
    console.log('Global teardown complete: database reset');
  } catch {
    // Ignore errors - backend may already be down
  }
}

export default globalTeardown;
