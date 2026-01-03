import { test as base, expect, type Page } from '@playwright/test';

/**
 * Shared fixtures for mainloop e2e tests.
 * Import { test, expect } from this file in your test files.
 */

export const test = base.extend<{
  /** Page with app loaded and ready */
  appPage: Page;
}>({
  appPage: async ({ page }, use) => {
    await page.goto('/');
    await page.waitForSelector('text=$ mainloop', { timeout: 15000 });
    // Wait for SSE connection
    await page.waitForTimeout(500);
    await use(page);
  }
});

export { expect };

/**
 * Helper to send a chat message and wait for response.
 */
export async function sendMessage(page: Page, message: string): Promise<string> {
  // Find and fill the input
  const input = page
    .locator('input[placeholder*="message"], textarea[placeholder*="message"]')
    .first();
  await input.fill(message);
  await input.press('Enter');

  // Wait for response (assistant message appears)
  // Look for the most recent assistant message
  await page.waitForTimeout(1000); // Brief wait for response to start

  // Get the last assistant message content
  const messages = page.locator('[data-role="assistant"], .assistant-message');
  const count = await messages.count();

  if (count > 0) {
    const lastMessage = messages.nth(count - 1);
    await lastMessage.waitFor({ state: 'visible', timeout: 30000 });
    return (await lastMessage.textContent()) || '';
  }

  return '';
}

/**
 * Helper to check conversation has N messages.
 */
export async function getMessageCount(page: Page): Promise<number> {
  const messages = page.locator(
    '[data-role="user"], [data-role="assistant"], .user-message, .assistant-message'
  );
  return await messages.count();
}
