import { defineConfig, devices } from '@playwright/test';

/**
 * Playwright configuration with layered test dependencies.
 *
 * Test stages (fail-fast, each depends on previous):
 *   1. setup   - App loads, API healthy
 *   2. basic   - Simple conversation back-and-forth
 *   3. context - Conversation history, compaction
 *   4. agents  - Spawning workers, task management
 *
 * Usage:
 *   make test-e2e       # Run all stages with isolated env
 *   make test-e2e-ui    # Interactive UI mode
 */
export default defineConfig({
  testDir: './tests',

  // Fail fast in CI, run all in dev
  maxFailures: process.env.CI ? 5 : 0,

  // Sequential by default for state-dependent tests
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  workers: 1, // Sequential execution to maintain state

  reporter: [['html', { open: 'never' }], ['list']],

  use: {
    baseURL: process.env.PLAYWRIGHT_BASE_URL || 'http://localhost:5173',
    trace: 'on-first-retry',
    screenshot: 'only-on-failure',
    video: 'retain-on-failure'
  },

  projects: [
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'] }
    },
    {
      name: 'mobile',
      testMatch: /mobile\/.*\.spec\.ts/,
      use: { ...devices['Pixel 5'] }
    }
  ],

  webServer: {
    command: 'VITE_API_URL=http://localhost:8081 pnpm dev --port 5173',
    url: 'http://localhost:5173',
    reuseExistingServer: !process.env.CI,
    timeout: 60000,
    stdout: 'ignore',
    stderr: 'pipe'
  }
});
