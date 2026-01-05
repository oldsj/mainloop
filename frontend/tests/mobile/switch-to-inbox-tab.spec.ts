// spec: frontend/specs/mobile-navigation.md
// seed: tests/seed.spec.ts

import { test, expect } from '@playwright/test';

test.describe('Tab Navigation', () => {
  test('Switch to Inbox Tab', async ({ page }) => {
    // Set mobile viewport (Pixel 5)
    await page.setViewportSize({ width: 393, height: 851 });

    await page.goto('/');

    // Wait for page to load
    await expect(page.getByRole('heading', { name: '$ mainloop' }).first()).toBeVisible();

    // Verify we start on [CHAT] tab
    const chatTab = page.getByRole('button', { name: 'Chat' });
    await expect(chatTab).toHaveClass(/text-term-accent/);

    // Tap the [INBOX] tab
    const inboxTab = page.getByRole('button', { name: 'Inbox' });
    await inboxTab.click();

    // Verify [INBOX] tab is highlighted (this proves tab switched)
    await expect(inboxTab).toHaveClass(/text-term-accent/);

    // Verify [CHAT] tab is muted
    await expect(chatTab).not.toHaveClass(/text-term-accent/);

    // Verify chat input is hidden (proves we're on inbox tab)
    await expect(page.getByPlaceholder('Enter command...').first()).not.toBeVisible();
  });
});
