import { defineConfig, devices } from '@playwright/test';

/**
 * Modern Playwright configuration with project dependencies.
 *
 * Environment variables:
 *   PLAYWRIGHT_BASE_URL - Frontend URL (default: http://localhost:5173)
 *   API_URL            - Backend URL (default: http://localhost:8000)
 *
 * Usage:
 *   make test          # Playwright UI (backend Docker, frontend Vite)
 *   make test-run      # Headless CI mode
 */

const baseURL = process.env.PLAYWRIGHT_BASE_URL || 'http://localhost:5173';
const apiURL = process.env.API_URL || 'http://localhost:8000';

// Headless by default (override with HEADLESS=false for debugging)
const headless = process.env.HEADLESS ? process.env.HEADLESS === 'true' : true;

export default defineConfig({
  testDir: './tests',

  // Lesson #5: Fail fast on early tests
  // Set maxFailures=1 in CI to stop immediately on first failure
  // Locally, run all tests to see full scope of issues
  maxFailures: process.env.CI ? 1 : undefined,

  // Parallel execution with per-worker schema isolation
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: 0, // No retries - fail fast, don't hide flakiness
  workers: undefined, // Per-project configuration

  reporter: process.env.CI
    ? [['github'], ['html', { open: 'never' }]]
    : [['list'], ['html', { open: 'never' }]],

  // Global timeout for tests
  timeout: 30000,
  expect: { timeout: 10000 },

  use: {
    baseURL,
    headless,
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    video: 'retain-on-failure',
    colorScheme: 'dark', // Reduce white flashing between tests
    // Pass API_URL to tests via extraHTTPHeaders or use in fixtures
    extraHTTPHeaders: {
      'x-test-api-url': apiURL
    }
  },

  // Store API_URL for fixtures to use
  metadata: {
    apiURL
  },

  projects: [
    // Lesson #5: Build tests incrementally - basic connectivity first
    // Stage 1: Fast desktop tests - UI and seeded data tests (no Claude API)
    {
      name: 'fast',
      testMatch: /(question-answering|basic\/0[2-3]).*\.spec\.ts/,
      use: { ...devices['Desktop Chrome'] },
      fullyParallel: true
    },

    // Stage 2: Fast mobile tests
    {
      name: 'mobile',
      testMatch: /mobile\/.*\.spec\.ts/,
      use: { ...devices['Pixel 5'] },
      fullyParallel: true
    },

    // Stage 3: Slow E2E tests - real Claude API interactions (run serially to avoid backend overload)
    {
      name: 'slow-e2e',
      testMatch: /(00-create-task|01-send-message|01-conversation-history).*\.spec\.ts/,
      use: { ...devices['Desktop Chrome'] },
      fullyParallel: false  // Run serially - Claude API can't handle parallel requests well
    }
  ],

  // Global setup/teardown (runs once, not per-project)
  globalSetup: './tests/global-setup.ts',
  globalTeardown: './tests/global-teardown.ts',

  // Start Vite dev server only if not using external frontend (Docker)
  // When PLAYWRIGHT_BASE_URL is set, we assume Docker frontend is running
  ...(process.env.PLAYWRIGHT_BASE_URL
    ? {}
    : {
        webServer: {
          command: 'pnpm dev --port 5173 --strictPort',
          url: 'http://localhost:5173',
          reuseExistingServer: true,
          timeout: 60000,
          env: {
            VITE_API_URL: apiURL
          }
        }
      })
});

// Export for use in fixtures
export { apiURL, baseURL };
