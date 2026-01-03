import { test, expect } from '@playwright/test';

/**
 * BASIC STAGE - Verify empty inbox state
 *
 * After basic conversation, inbox should still be empty.
 */

test.describe('Basic: Empty Inbox', () => {
  test('inbox shows empty state', async ({ page }) => {
    await page.goto('/');

    // Verify inbox panel is visible
    await expect(page.getByRole('heading', { name: '[INBOX]' })).toBeVisible();

    // Verify empty state shows terminal-style command prompt
    await expect(page.getByText('$ ls inbox/')).toBeVisible();

    // Verify empty state message 'All caught up' is displayed
    await expect(page.getByText('All caught up')).toBeVisible();
  });
});
