// spec: frontend/specs/mobile-navigation.md
// seed: tests/seed.spec.ts

import { test, expect } from '@playwright/test';

test.describe('Tab Navigation', () => {
  test('Switch to Inbox Tab', async ({ page }) => {
    // Set mobile viewport (Pixel 5)
    await page.setViewportSize({ width: 393, height: 851 });

    // 1. Start on [CHAT] tab (default)
    await page.goto('/');

    // Wait for page to load
    await expect(page.getByRole('heading', { name: '$ mainloop' })).toBeVisible();

    // 2. Tap the [INBOX] tab
    const inboxTab = page.getByRole('button', { name: 'Inbox' });
    await inboxTab.click();

    // Expected: [INBOX] tab becomes highlighted (text-term-accent color)
    await expect(inboxTab).toHaveClass(/text-term-accent/);

    // Expected: [CHAT] tab becomes muted
    const chatTab = page.getByRole('button', { name: 'Chat' });
    await expect(chatTab).not.toHaveClass(/text-term-accent/);

    // Expected: Inbox panel fills main content area
    await expect(page.getByRole('heading', { name: '[INBOX]' })).toBeVisible();

    // Expected: Chat view hidden completely
    await expect(page.getByText('Start a conversation to begin')).not.toBeVisible();

    // Expected: Input bar hidden (inbox has its own interactions)
    await expect(page.getByRole('textbox', { name: 'Enter command...' })).not.toBeVisible();
  });
});
