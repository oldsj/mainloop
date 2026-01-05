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

    // Verify we're on chat tab by default
    const chatTab = page.getByRole('button', { name: 'Chat' });
    await expect(chatTab).toHaveClass(/text-term-accent/);

    // 1. Start typing a message in chat
    const inputField = page.getByTestId('command-input').nth(1); // Get the second one (mobile layout)
    await inputField.fill('test message draft');
    await expect(inputField).toHaveValue('test message draft');

    // 2. Switch to [INBOX] tab
    const inboxTab = page.getByRole('button', { name: 'Inbox' });
    await inboxTab.click();
    await expect(inboxTab).toHaveClass(/text-term-accent/, { timeout: 2000 });

    // 3. Switch back to [CHAT] tab
    await chatTab.click();
    await expect(chatTab).toHaveClass(/text-term-accent/, { timeout: 2000 });

    // Expected: Typed text preserved in input field
    await expect(inputField).toHaveValue('test message draft');
  });
});
