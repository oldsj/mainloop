import { test as base, expect } from '@playwright/test';

/**
 * Planning workflow journey - single session, shared page.
 *
 * This test uses the shared page pattern to avoid flakiness:
 * - One page/context for all tests
 * - Each test builds on previous state
 * - No parallel execution issues
 *
 * Run locally: pnpm exec playwright test --project=planning
 * (Skipped in CI - too flaky with real Claude API)
 */

const apiURL = process.env.API_URL || 'http://localhost:8000';
const fixtureRepoUrl = 'https://github.com/test/fixture-repo';

// Extend test to use shared page across all tests
const test = base.extend<{ sharedPage: ReturnType<typeof base.info>['_page'] }>({
  sharedPage: [
    async ({ browser }, use) => {
      const context = await browser.newContext();
      const page = await context.newPage();

      // Set up user isolation
      const userId = `test-${Date.now()}-${Math.random().toString(36).slice(2, 11)}`;

      await page.route(`${apiURL}/**`, async (route) => {
        const url = new URL(route.request().url());
        if (url.pathname.includes('/events') || url.pathname.includes('/stream')) {
          url.searchParams.set('user_id_query', userId);
          await route.continue({ url: url.toString() });
          return;
        }
        const headers = { ...route.request().headers(), 'X-User-ID': userId };
        await route.continue({ headers });
      });

      // Seed the fixture repo
      const seedResponse = await page.request.post(`${apiURL}/internal/test/seed-repo`, {
        data: { owner: 'test', name: 'fixture-repo' }
      });
      if (!seedResponse.ok()) {
        throw new Error(`Failed to seed repo: ${seedResponse.status()}`);
      }

      await page.goto('/');
      await expect(page.getByRole('heading', { name: '$ mainloop' })).toBeVisible({
        timeout: 15000
      });

      await use(page);
      await context.close();
    },
    { scope: 'worker' }
  ]
});

test.describe('Planning Journey', () => {
  test.describe.configure({ mode: 'serial' });
  test.setTimeout(180000); // 3 min for full journey

  test('1. sanity check - chat works', async ({ sharedPage: page }) => {
    const input = page.getByPlaceholder('Enter command...').first();
    const execButton = page.getByRole('button', { name: 'EXEC' });

    await expect(input).toBeEnabled();
    await input.fill('What is 2 + 2?');
    await execButton.click();

    // Verify submission
    await expect(page.getByText('What is 2 + 2?').first()).toBeVisible({ timeout: 10000 });

    // Wait for response
    const response = page.locator('.message.bg-term-bg-secondary').last();
    await expect(response).toBeVisible({ timeout: 60000 });

    const text = await response.textContent();
    expect(text).toMatch(/4|four/i);
  });

  test('2. ask about planning triggers helpful response', async ({ sharedPage: page }) => {
    const input = page.getByPlaceholder('Enter command...').first();
    const execButton = page.getByRole('button', { name: 'EXEC' });

    await expect(input).toBeEnabled({ timeout: 10000 });
    await input.fill('I want to add a feature to a GitHub repo. What do you need to know?');
    await execButton.click();

    await expect(page.getByText('add a feature').first()).toBeVisible({ timeout: 10000 });

    const messages = page.locator('.message.bg-term-bg-secondary');
    const count = await messages.count();
    await expect(messages).toHaveCount(count + 1, { timeout: 60000 });

    const text = await messages.last().textContent();
    expect(text?.toLowerCase()).toMatch(/repository|repo|github|url|which/i);
  });

  test('3. provide repo URL - planning starts', async ({ sharedPage: page }) => {
    const input = page.getByPlaceholder('Enter command...').first();
    const execButton = page.getByRole('button', { name: 'EXEC' });

    await expect(input).toBeEnabled({ timeout: 10000 });
    await input.fill(`The repo is ${fixtureRepoUrl}. Start planning to add a multiply function.`);
    await execButton.click();

    await expect(page.getByText('Start planning').first()).toBeVisible({ timeout: 10000 });

    const messages = page.locator('.message.bg-term-bg-secondary');
    const count = await messages.count();
    await expect(messages).toHaveCount(count + 1, { timeout: 120000 });

    // Claude should acknowledge the repo or start exploring
    const text = await messages.last().textContent();
    expect(text?.toLowerCase()).toMatch(/planning|fixture-repo|explore|codebase|main\.py|utils/i);
  });

  test('4. follow-up about code - context maintained', async ({ sharedPage: page }) => {
    const input = page.getByPlaceholder('Enter command...').first();
    const execButton = page.getByRole('button', { name: 'EXEC' });

    await expect(input).toBeEnabled({ timeout: 10000 });
    await input.fill('What functions already exist in the codebase?');
    await execButton.click();

    await expect(page.getByText('functions already exist').first()).toBeVisible({ timeout: 10000 });

    const messages = page.locator('.message.bg-term-bg-secondary');
    const count = await messages.count();
    await expect(messages).toHaveCount(count + 1, { timeout: 120000 });

    // Claude should reference actual code from fixture repo
    const text = await messages.last().textContent();
    expect(text?.toLowerCase()).toMatch(
      /hello|add|format_name|validate_email|main\.py|utils\.py|function/i
    );
  });
});
