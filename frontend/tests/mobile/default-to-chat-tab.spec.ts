import { test, expect } from '../fixtures';

test.describe('Tab Navigation', () => {
  test('Default to Chat Tab', async ({ appPage: page }) => {
    // Input bar visible at bottom (above tab bar)
    await expect(page.getByRole('textbox', { name: 'Enter command...' })).toBeVisible();

    // [CHAT] tab is highlighted/active by default
    const chatTab = page.getByRole('button', { name: 'Chat' });
    await expect(chatTab).toBeVisible();
    await expect(chatTab).toHaveClass(/text-term-accent/);

    // Inbox view hidden
    const inboxHeading = page.getByRole('heading', { name: '[INBOX]' });
    await expect(inboxHeading).not.toBeVisible();
  });
});
