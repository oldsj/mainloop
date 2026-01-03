// spec: frontend/specs/mobile-navigation.md
// seed: tests/seed.spec.ts

import { test, expect } from '@playwright/test';

test.describe('Tab Navigation', () => {
  test('Return to Chat Tab', async ({ page }) => {
    // Set mobile viewport (Pixel 5)
    await page.setViewportSize({ width: 393, height: 851 });

    await page.goto('http://localhost:3031');

    // Wait for page to load
    await expect(page.getByRole('heading', { name: '$ mainloop' })).toBeVisible();

    // 1. Navigate to [INBOX] tab
    const inboxTab = page.getByRole('button', { name: 'Inbox' });
    await inboxTab.click();
    await expect(page.getByRole('heading', { name: '[INBOX]' })).toBeVisible();

    // 2. Tap the [CHAT] tab
    const chatTab = page.getByRole('button', { name: 'Chat' });
    await chatTab.click();
    
    // Expected: [CHAT] tab becomes highlighted
    await expect(chatTab).toHaveClass(/text-term-accent/);
    
    // Expected: [INBOX] tab becomes muted
    await expect(inboxTab).not.toHaveClass(/text-term-accent/);

    // Expected: Input bar visible again (proves chat view is active)
    await expect(page.getByRole('textbox', { name: 'Enter command...' })).toBeVisible();
  });
});
