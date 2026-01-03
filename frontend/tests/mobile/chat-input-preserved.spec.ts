// spec: frontend/specs/mobile-navigation.md
// seed: tests/seed.spec.ts

import { test, expect } from '@playwright/test';

test.describe('State Persistence', () => {
  // FIXME: Chat input is not preserved when switching tabs in mobile view.
  // The mobile layout unmounts the chat component when switching to inbox tab,
  // causing the input value to be lost. Need to either:
  // 1. Use CSS visibility instead of conditional rendering, or
  // 2. Store draft message in a persisted store
  test.fixme('Chat Input Preserved', async ({ page }) => {
    // Set mobile viewport (Pixel 5)
    await page.setViewportSize({ width: 393, height: 851 });
    await page.goto('http://localhost:3031');
    
    // Wait for page to load
    await expect(page.getByRole('heading', { name: '$ mainloop' })).toBeVisible();
    
    // 1. Start typing a message in chat
    const inputField = page.getByRole('textbox', { name: 'Enter command...' });
    await inputField.fill('test message draft');
    await expect(inputField).toHaveValue('test message draft');
    
    // 2. Switch to [INBOX] tab
    const inboxTab = page.getByRole('button', { name: 'Inbox' });
    await inboxTab.click();
    await expect(page.getByRole('heading', { name: '[INBOX]' })).toBeVisible();

    // 3. Switch back to [CHAT] tab
    const chatTab = page.getByRole('button', { name: 'Chat' });
    await chatTab.click();
    await expect(inputField).toBeVisible();
    
    // Expected: Typed text preserved in input field
    // Expected: No loss of draft message
    await expect(inputField).toHaveValue('test message draft');
  });
});
