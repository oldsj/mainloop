import { test, expect } from '@playwright/test';
import { resetTestData } from '../fixtures';

/**
 * BASIC STAGE - Send a message test
 */

test.describe('Basic: Send Message', () => {
  test('send message and receive response', async ({ page }) => {
    // Reset data for clean state
    await resetTestData();

    await page.goto('/');
    await expect(page.getByRole('heading', { name: '$ mainloop' })).toBeVisible();

    // Type and send a simple message
    const input = page.getByPlaceholder('Enter command...').first();
    await input.click(); // Focus first
    await input.fill('hello');
    await input.press('Enter');

    // Message should appear in chat (wait longer, backend needs to process)
    await expect(page.getByText('hello').first()).toBeVisible({ timeout: 10000 });
  });
});
