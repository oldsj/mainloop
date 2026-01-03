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

  // Fail fast - stop on first failure
  maxFailures: process.env.CI ? 5 : 1,

  // Sequential by default for state-dependent tests
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  workers: 1, // Sequential execution to maintain state

  reporter: [['html', { open: 'never' }], ['list']],

  use: {
    baseURL: process.env.PLAYWRIGHT_BASE_URL || 'http://localhost:3031',
    trace: 'on-first-retry',
    screenshot: 'only-on-failure',
    video: 'retain-on-failure',
  },

  projects: [
    // Agents project - for test-agents MCP server
    {
      name: 'agents',
      testMatch: /seed\.spec\.ts/,
      use: {
        ...devices['Desktop Chrome'],
        viewport: { width: 1280, height: 720 },
      },
    },

    // Stage 1: Setup - verify app loads and API is healthy
    {
      name: 'setup',
      testMatch: /.*\.setup\.ts/,
      use: {
        ...devices['Desktop Chrome'],
        viewport: { width: 1280, height: 720 },
      },
    },

    // Stage 2: Basic - simple conversation (depends on setup)
    {
      name: 'basic',
      testMatch: /basic\/.*\.spec\.ts/,
      dependencies: ['setup'],
      use: {
        ...devices['Desktop Chrome'],
        viewport: { width: 1280, height: 720 },
      },
    },

    // Stage 3: Context - conversation history and compaction (depends on basic)
    {
      name: 'context',
      testMatch: /context\/.*\.spec\.ts/,
      dependencies: ['basic'],
      use: {
        ...devices['Desktop Chrome'],
        viewport: { width: 1280, height: 720 },
      },
    },

    // Stage 4: Agents - worker spawning and task management (depends on context)
    {
      name: 'agents',
      testMatch: /agents\/.*\.spec\.ts/,
      dependencies: ['context'],
      use: {
        ...devices['Desktop Chrome'],
        viewport: { width: 1280, height: 720 },
      },
    },

    // Mobile tests run after all desktop tests pass
    {
      name: 'mobile',
      testMatch: /mobile\/.*\.spec\.ts/,
      dependencies: ['basic'],
      use: {
        ...devices['Pixel 5'],
        viewport: { width: 393, height: 851 }, // Pixel 5 viewport
      },
    },
  ],

  webServer: {
    command: 'echo "Waiting for test environment..."',
    url: process.env.PLAYWRIGHT_BASE_URL || 'http://localhost:3031',
    reuseExistingServer: true,
    timeout: 60000,
  },
});
