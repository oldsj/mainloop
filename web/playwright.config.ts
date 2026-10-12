import { defineConfig, devices } from '@playwright/test';

/**
 * Config only: the journeys arrive with F9. They run against `VITE_MSW=1 pnpm dev`, on a
 * desktop and a Pixel 5 (393×851) project, so both shells are covered.
 */
export default defineConfig({
  testDir: './e2e',
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? 'github' : 'list',
  use: { baseURL: 'http://127.0.0.1:5173', trace: 'on-first-retry' },
  webServer: {
    command: 'VITE_MSW=1 node_modules/.bin/vite --host 127.0.0.1 --port 5173',
    url: 'http://127.0.0.1:5173',
    reuseExistingServer: !process.env.CI
  },
  projects: [
    { name: 'desktop', use: { ...devices['Desktop Chrome'] } },
    { name: 'mobile', use: { ...devices['Pixel 5'] } }
  ]
});
