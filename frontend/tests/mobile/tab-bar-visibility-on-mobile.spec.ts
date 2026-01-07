import { test, expect } from '../fixtures';

test.describe('Tab Bar Display', () => {
  test('Tab Bar Visibility on Mobile', async ({ appPage: page }) => {
    // Bottom tab bar visible with two tabs
    const chatTab = page.getByRole('button', { name: 'Chat' });
    const inboxTab = page.getByRole('button', { name: 'Inbox' });

    await expect(chatTab).toBeVisible();
    await expect(inboxTab).toBeVisible();

    // Terminal-style bracketed labels
    await expect(chatTab).toContainText('[CHAT]');
    await expect(inboxTab).toContainText('[INBOX]');
  });
});
