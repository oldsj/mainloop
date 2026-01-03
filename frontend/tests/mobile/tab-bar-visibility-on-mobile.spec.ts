// spec: frontend/specs/mobile-navigation.md
// seed: tests/seed.spec.ts

import { test, expect } from '@playwright/test';

test.describe('Tab Bar Display', () => {
  test('Tab Bar Visibility on Mobile', async ({ page }) => {
    // Set mobile viewport (Pixel 5)
    await page.setViewportSize({ width: 393, height: 851 });

    // 1. Load application at mobile viewport
    await page.goto('http://localhost:3031');

    // 2. Wait for app to load
    await expect(page.getByRole('heading', { name: '$ mainloop' })).toBeVisible();

    // Expected: Bottom tab bar visible at bottom of screen
    // Expected: Two tabs: [CHAT] and [INBOX]
    const chatTab = page.getByRole('button', { name: 'Chat' });
    const inboxTab = page.getByRole('button', { name: 'Inbox' });
    
    await expect(chatTab).toBeVisible();
    await expect(inboxTab).toBeVisible();
    
    // Expected: Terminal-style bracketed labels
    await expect(chatTab).toContainText('[CHAT]');
    await expect(inboxTab).toContainText('[INBOX]');
  });
});
