import { test, expect } from '@playwright/test';
import { setupConversation } from '../fixtures';

/**
 * BASIC STAGE - Send a message test
 */

test.describe.configure({ mode: 'serial' }); // Real Claude API calls must run serially

test.describe('Basic: Send Message', () => {
  test('send message and receive response', async ({ page }) => {
    await page.goto('/');

    // Use the setupConversation fixture which handles the full flow
    await setupConversation(page);

    // Verify message appeared
    await expect(page.getByText('hello').first()).toBeVisible();

    // Verify we have at least 2 messages (user + assistant)
    const messages = page.locator('.message');
    expect(await messages.count()).toBeGreaterThanOrEqual(2);
  });
});
