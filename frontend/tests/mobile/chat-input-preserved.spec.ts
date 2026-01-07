// spec: frontend/specs/mobile-navigation.md
// seed: tests/seed.spec.ts

import { test, expect } from '@playwright/test';

test.describe('State Persistence', () => {
  test('Chat Input Preserved', async ({ page }) => {
    // Set mobile viewport (Pixel 5)
    await page.setViewportSize({ width: 393, height: 851 });
    await page.goto('/');

    // Wait for page to load
    await expect(page.getByRole('heading', { name: '$ mainloop' }).first()).toBeVisible();

    const chatTab = page.getByRole('button', { name: 'Chat' });
    const inboxTab = page.getByRole('button', { name: 'Inbox' });
    const inputField = page.getByRole('textbox', { name: 'Enter command...' });

    // 1. Start typing a message in chat
    await expect(inputField).toBeVisible();
    await inputField.fill('test message draft');
    await expect(inputField).toHaveValue('test message draft');

    // 2. Switch to [INBOX] tab - verify by content change
    await inboxTab.click();
    await expect(inputField).not.toBeVisible();
    await expect(page.getByRole('heading', { name: '[INBOX]' })).toBeVisible();

    // 3. Switch back to [CHAT] tab - verify input returns
    await chatTab.click();
    await expect(inputField).toBeVisible();

    // Expected: Typed text preserved in input field
    await expect(inputField).toHaveValue('test message draft');
  });
});
