// spec: frontend/specs/mobile-navigation.md
// seed: tests/seed.spec.ts

import { test, expect } from '@playwright/test';

test.describe('Tab Navigation', () => {
  test('Default to Chat Tab', async ({ page }) => {
    // Set mobile viewport (Pixel 5)
    await page.setViewportSize({ width: 393, height: 851 });

    // 1. Load application fresh at mobile viewport
    await page.goto('http://localhost:3031');

    // 2. Observe which view is active
    // Expected: Input bar at bottom (above tab bar)
    await expect(page.getByRole('textbox', { name: 'Enter command...' })).toBeVisible();

    // Expected: [CHAT] tab is highlighted/active by default
    const chatTab = page.getByRole('button', { name: 'Chat' });
    await expect(chatTab).toBeVisible();
    await expect(chatTab).toHaveClass(/text-term-accent/);
    
    // Expected: Inbox view hidden
    const inboxHeading = page.getByRole('heading', { name: '[INBOX]' });
    await expect(inboxHeading).not.toBeVisible();
  });
});
