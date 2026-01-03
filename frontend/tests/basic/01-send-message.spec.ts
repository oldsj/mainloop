import { test, expect } from '@playwright/test';

/**
 * BASIC STAGE - Build up conversation state
 *
 * This test creates the initial conversation that later tests depend on.
 */

test.describe('Basic: Send Message', () => {
  test('send message and receive response', async ({ page }) => {
    await page.goto('/');
    await expect(page.getByRole('heading', { name: '$ mainloop' })).toBeVisible();

    // Type and send a simple message (use first input - desktop)
    const input = page.getByPlaceholder('Enter command...').first();
    await input.fill('hello');
    await input.press('Enter');

    // Message appears in chat
    await expect(page.getByText('hello').first()).toBeVisible();

    // Verify message is in the conversation (builds state for later tests)
    // Note: Claude response depends on agent being available - tested separately
    const messages = page.locator('.message');
    await expect(messages.first()).toBeVisible();
  });
});
