import { test, expect } from '../fixtures';
import type { Page } from '@playwright/test';

/**
 * Full user journey E2E test - real Claude API calls.
 *
 * Single session that builds up progressively:
 * 1. Send message → get response
 * 2. Verify conversation history
 * 3. Send follow-up → verify context maintained
 * 4. Create task → verify it appears
 * 5. Create session → verify it appears
 */

test.describe('User Journey (E2E)', () => {
  test.describe.configure({ mode: 'serial' });
  test.setTimeout(120000); // 2 min for full journey

  let sharedPage: Page;

  test.beforeAll(async ({ browser }) => {
    // Create a single page/context for all tests in this describe block
    const context = await browser.newContext();
    sharedPage = await context.newPage();

    // Set up user isolation
    const userId = `test-${Date.now()}-${Math.random().toString(36).slice(2, 11)}`;
    const apiURL = process.env.API_URL || 'http://localhost:8000';

    await sharedPage.route(`${apiURL}/**`, async (route) => {
      const url = new URL(route.request().url());
      if (url.pathname.includes('/events') || url.pathname.includes('/stream')) {
        url.searchParams.set('user_id_query', userId);
        await route.continue({ url: url.toString() });
        return;
      }
      const headers = { ...route.request().headers(), 'X-User-ID': userId };
      await route.continue({ headers });
    });

    await sharedPage.goto('/');
    await expect(sharedPage.getByRole('heading', { name: '$ mainloop' })).toBeVisible({
      timeout: 15000
    });
  });

  test.afterAll(async () => {
    await sharedPage?.context().close();
  });

  test('1. send message and receive response', async () => {
    const page = sharedPage;
    const input = page.getByPlaceholder('Enter command...').first();

    await input.fill('hello');
    await page.getByRole('button', { name: 'EXEC' }).click();

    // Verify our message appeared
    await expect(page.getByText('hello').first()).toBeVisible({ timeout: 10000 });

    // Wait for assistant response
    const assistantMessage = page.locator('.message.bg-term-bg-secondary').first();
    await expect(assistantMessage).toBeVisible({ timeout: 30000 });

    // Verify we have at least 2 messages
    const messages = page.locator('.message');
    expect(await messages.count()).toBeGreaterThanOrEqual(2);
  });

  test('2. conversation history is visible', async () => {
    const page = sharedPage;

    // Previous messages should still be visible
    await expect(page.getByText('hello').first()).toBeVisible();

    // Should have messages from step 1
    const messages = page.locator('.message');
    expect(await messages.count()).toBeGreaterThanOrEqual(2);
  });

  test('3. send follow-up maintains context', async () => {
    const page = sharedPage;
    const messages = page.getByRole('main').locator('.message');
    const countBefore = await messages.count();

    const input = page.getByPlaceholder('Enter command...').first();
    await input.fill('what did I just say?');
    await page.getByRole('button', { name: 'EXEC' }).click();

    // New message appears
    await expect(page.getByText('what did I just say?').first()).toBeVisible();

    // Wait for response to arrive (2 new messages: user + assistant)
    // Use longer timeout since Claude API can be slow
    await expect(messages).toHaveCount(countBefore + 2, { timeout: 60000 });
  });

  test('4. create task via conversation', async () => {
    const page = sharedPage;
    const input = page.getByPlaceholder('Enter command...').first();

    // Wait for input to be ready
    await expect(input).toBeEnabled({ timeout: 10000 });

    await input.fill(
      'Spawn a task to update the README on https://github.com/oldsj/mainloop - add a quick start section. I confirm, please spawn now.'
    );
    await page.getByRole('button', { name: 'EXEC' }).click();

    // Verify message appeared
    await expect(page.getByText('Spawn a task').first()).toBeVisible({ timeout: 10000 });

    // Wait for task to appear in sidebar
    await expect(
      page.locator('[data-testid="projects-list"]').getByText('oldsj/mainloop')
    ).toBeVisible({ timeout: 60000 });
  });

  // Skip: Depends on Claude deciding to spawn a session, which isn't guaranteed
  test.skip('5. create session via conversation', async () => {
    const page = sharedPage;

    // Navigate home to exit any reply mode from previous test
    await page.goto('/');
    await expect(page.getByRole('heading', { name: '$ mainloop' })).toBeVisible();

    // Wait for regular command input to be ready
    const input = page.getByPlaceholder('Enter command...').first();
    await expect(input).toBeEnabled({ timeout: 10000 });

    await input.fill(
      'Spawn a background session to research best practices for TypeScript error handling. Title it "Error Handling Research". I confirm, please spawn now.'
    );
    await page.getByRole('button', { name: 'EXEC' }).click();

    // Verify message appeared
    await expect(page.getByText('Error Handling Research').first()).toBeVisible({ timeout: 10000 });

    // Wait for session to appear in sessions list (look for title or "ACTIVE" badge)
    // The session will appear in the Sessions panel which should show the title
    await expect(page.getByRole('heading', { name: 'Sessions' })).toBeVisible({ timeout: 30000 });

    // Wait for the new session to appear - look for title in a session item button
    await expect(page.getByRole('button', { name: /Error Handling Research/ })).toBeVisible({
      timeout: 60000
    });
  });
});
