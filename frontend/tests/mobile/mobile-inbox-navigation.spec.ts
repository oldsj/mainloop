// spec: frontend/specs/inbox-management.md
// seed: frontend/tests/seed.spec.ts

import { test, expect } from '@playwright/test';

test.describe('Inbox Panel Visibility', () => {
  test('Mobile Inbox Tab Navigation', async ({ page }) => {
    // Set mobile viewport (Pixel 5)
    await page.setViewportSize({ width: 393, height: 851 });

    // 1. Load application at mobile viewport
    await page.goto('/');

    // 2. Wait for app to fully load
    await expect(page.getByRole('heading', { name: '$ mainloop' })).toBeVisible();

    // 2. Verify bottom tab bar is visible with [CHAT] and [INBOX] tabs
    const chatTab = page.getByRole('button', { name: 'Chat' });
    const inboxTab = page.getByRole('button', { name: 'Inbox' });
    await expect(chatTab).toBeVisible();
    await expect(inboxTab).toBeVisible();
    
    // Verify [CHAT] tab is active by default
    await expect(chatTab).toHaveClass(/text-term-accent/);

    // 3. Tap the [INBOX] tab
    await inboxTab.click();
    
    // Verify inbox panel is now visible
    await expect(page.getByRole('heading', { name: '[INBOX]' })).toBeVisible();
    
    // Verify [INBOX] tab is now active
    await expect(inboxTab).toHaveClass(/text-term-accent/);

    // Verify chat input is hidden when inbox is active (proves chat view is hidden)
    await expect(page.getByRole('textbox', { name: 'Enter command...' })).not.toBeVisible();
  });
});
