import { request } from '@playwright/test';

const apiURL = process.env.API_URL || 'http://localhost:8000';

/**
 * Global teardown - runs once after all tests complete.
 *
 * Lesson #6: Clean state management - forcefully clean state between runs.
 * This ensures no test artifacts are left behind.
 */
async function globalTeardown() {
  try {
    const requestContext = await request.newContext();

    // Reset database
    await requestContext.post(`${apiURL}/internal/test/reset`);

    // Note: Namespace cleanup is handled by the Kind reset script
    // to avoid needing kubectl access from Node.js

    await requestContext.dispose();
    console.log('Global teardown complete: database reset');
  } catch {
    // Ignore errors - backend may already be down
  }
}

export default globalTeardown;
