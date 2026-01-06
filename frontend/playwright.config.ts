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

// Headed by default locally, headless on CI (override with HEADLESS=true/false)
const headless = process.env.HEADLESS ? process.env.HEADLESS === 'true' : !!process.env.CI;

export default defineConfig({
  testDir: './tests',

  // Run all tests - don't stop on first failure (see all issues)
  maxFailures: undefined,

  // Tests run serially due to shared DB reset and real Claude API
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: 0, // No retries - fail fast, don't hide flakiness
  workers: 1, // Bottleneck: resetTestData() and real Claude API calls

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
    // Desktop Chrome tests (default)
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'] },
      testIgnore: [/mobile\//, /global\.(setup|teardown)\.ts/]
    },

    // Mobile tests (Pixel 5 viewport)
    {
      name: 'mobile',
      use: { ...devices['Pixel 5'] },
      testMatch: /mobile\/.*\.spec\.ts/
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
