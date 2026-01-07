import { test, expect, apiURL } from '../fixtures';

/**
 * Full user journey E2E test - real Claude API calls.
 *
 * Single session that builds up progressively:
 * 1. Send message → get response
 * 2. Verify conversation history
 * 3. Send follow-up → verify context maintained
 * 4. Create task → verify it appears
 */

test.describe('User Journey (E2E)', () => {
  test.describe.configure({ mode: 'serial' });
  test.setTimeout(120000); // 2 min for full journey

  let sharedPage: ReturnType<(typeof test)['info']>['_page'];

  test.beforeAll(async ({ browser }) => {
    // Create a single page/context for all tests in this describe block
    const context = await browser.newContext();
    sharedPage = await context.newPage();

    // Set up user isolation
    const userId = `test-${Date.now()}-${Math.random().toString(36).slice(2, 11)}`;

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
    await input.press('Enter');

    // New message appears
    await expect(page.getByText('what did I just say?').first()).toBeVisible();

    // Wait for response
    await expect(page.getByRole('main').getByText('processing')).toBeHidden({ timeout: 60000 });

    // Verify response arrived (2 new messages: user + assistant)
    await expect(messages).toHaveCount(countBefore + 2, { timeout: 5000 });
  });

  // Skip: Requires external network access to GitHub for planning mode repo cache
  // The new in-thread planning flow clones repos before planning, which fails in Kind cluster
  test.skip('4. create task via conversation', async () => {
    const page = sharedPage;
    const input = page.getByPlaceholder('Enter command...').first();

    await input.fill(
      'Spawn a task to update the README on https://github.com/oldsj/mainloop - add a quick start section. I confirm, please spawn now.'
    );
    await input.press('Enter');

    // Wait for task to appear in sidebar
    await expect(
      page.locator('[data-testid="projects-list"]').getByText('oldsj/mainloop')
    ).toBeVisible({ timeout: 30000 });
  });
});
