// spec: frontend/specs/mobile-navigation.md
// seed: tests/seed.spec.ts

import { test, expect } from '@playwright/test';

test.describe('Tab Navigation', () => {
  test('Return to Chat Tab', async ({ page }) => {
    // Set mobile viewport (Pixel 5)
    await page.setViewportSize({ width: 393, height: 851 });

    await page.goto('/');

    // Wait for page to load
    await expect(page.getByRole('heading', { name: '$ mainloop' }).first()).toBeVisible();

    const chatTab = page.getByRole('button', { name: 'Chat' });
    const inboxTab = page.getByRole('button', { name: 'Inbox' });
    const chatInput = page.getByRole('textbox', { name: 'Enter command...' });

    // Verify we start on chat tab (input visible)
    await expect(chatInput).toBeVisible();

    // Navigate to [INBOX] tab - verify by content change, not CSS
    await inboxTab.click();
    await expect(page.getByRole('heading', { name: '[INBOX]' })).toBeVisible();
    await expect(chatInput).not.toBeVisible();

    // Return to [CHAT] tab - verify input is visible again
    await chatTab.click();
    await expect(chatInput).toBeVisible();
  });
});
