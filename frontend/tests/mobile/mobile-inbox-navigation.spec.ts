// spec: frontend/specs/inbox-management.md
// seed: frontend/tests/seed.spec.ts

import { test, expect } from '../fixtures';

test.describe('Inbox Panel Visibility', () => {
  test('Mobile Inbox Tab Navigation', async ({ appPage: page }) => {
    // Note: Mobile viewport already set by project config (Pixel 5)

    // 2. Verify bottom tab bar is visible with [CHAT] and [INBOX] tabs
    const chatTab = page.getByRole('button', { name: 'Chat' });
    const inboxTab = page.getByRole('button', { name: 'Inbox' });
    await expect(chatTab).toBeVisible();
    await expect(inboxTab).toBeVisible();

    // Verify [CHAT] tab is active by default
    await expect(chatTab).toHaveClass(/text-term-accent/);

    // 3. Tap the [INBOX] tab
    await inboxTab.click();

    // Verify [INBOX] tab is now active (proves tab switched)
    await expect(inboxTab).toHaveClass(/text-term-accent/, { timeout: 2000 });

    // Verify chat tab is no longer active
    await expect(chatTab).not.toHaveClass(/text-term-accent/);

    // Verify chat input is hidden when inbox is active (proves chat view is hidden)
    await expect(page.getByPlaceholder('Enter command...').first()).not.toBeVisible();
  });
});
