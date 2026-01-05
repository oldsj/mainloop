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

    const inboxTab = page.getByRole('button', { name: 'Inbox' });
    const chatTab = page.getByRole('button', { name: 'Chat' });

    // Navigate to [INBOX] tab
    await inboxTab.click();
    await expect(inboxTab).toHaveClass(/text-term-accent/, { timeout: 2000 });

    // Return to [CHAT] tab
    await chatTab.click();
    await expect(chatTab).toHaveClass(/text-term-accent/, { timeout: 2000 });
    await expect(inboxTab).not.toHaveClass(/text-term-accent/);
  });
});
