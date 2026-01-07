// spec: frontend/specs/inbox-management.md
// seed: frontend/tests/seed.spec.ts

import { test, expect } from '../fixtures';

test.describe('Inbox Panel Visibility', () => {
  test('Mobile Inbox Tab Navigation', async ({ appPage: page }) => {
    // Note: Mobile viewport already set by project config (Pixel 5)

    // Verify bottom tab bar is visible with [CHAT] and [INBOX] tabs
    const chatTab = page.getByRole('button', { name: 'Chat' });
    const inboxTab = page.getByRole('button', { name: 'Inbox' });
    const chatInput = page.getByRole('textbox', { name: 'Enter command...' });
    const inboxHeading = page.getByRole('heading', { name: '[INBOX]' });

    await expect(chatTab).toBeVisible();
    await expect(inboxTab).toBeVisible();

    // Verify we start on chat tab (input visible, inbox hidden)
    await expect(chatInput).toBeVisible();
    await expect(inboxHeading).not.toBeVisible();

    // Tap the [INBOX] tab
    await inboxTab.click();

    // Verify inbox is now showing (proves tab switched)
    await expect(inboxHeading).toBeVisible();

    // Verify chat input is hidden when inbox is active
    await expect(chatInput).not.toBeVisible();
  });
});
