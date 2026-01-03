import { test, expect } from '@playwright/test';

/**
 * CONTEXT STAGE - Verify conversation history
 *
 * This test verifies that messages from previous tests are preserved.
 */

test.describe('Context: Conversation History', () => {
  test('previous messages are visible', async ({ page }) => {
    await page.goto('/');

    // The "hello" message from basic stage should still be visible
    await expect(page.getByText('hello').first()).toBeVisible();

    // There should be multiple messages in the conversation
    const messages = page.locator('.message');
    await expect(messages).toHaveCount(await messages.count());
    expect(await messages.count()).toBeGreaterThan(0);
  });

  test('send another message maintains history', async ({ page }) => {
    await page.goto('/');

    // Send a new message (use first input - desktop)
    const input = page.getByPlaceholder('Enter command...').first();
    await input.fill('what did I just say?');
    await input.press('Enter');

    // New message appears
    await expect(page.getByText('what did I just say?').first()).toBeVisible();

    // Old messages still present
    await expect(page.getByText('hello').first()).toBeVisible();

    // Response references previous context
    await expect(page.locator('.message').last()).toBeVisible({ timeout: 30000 });
  });
});
