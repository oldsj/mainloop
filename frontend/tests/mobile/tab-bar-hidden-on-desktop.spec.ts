import { test, expect } from '../fixtures';

test.describe('Tab Bar Display', () => {
  test('Tab Bar Hidden on Desktop', async ({ appPage: page }) => {
    // Override to desktop viewport for this test
    await page.setViewportSize({ width: 1280, height: 720 });
    await page.reload();
    await expect(page.getByRole('heading', { name: '$ mainloop' })).toBeVisible();

    // Inbox panel always visible on desktop
    await expect(page.getByRole('heading', { name: '[INBOX]' })).toBeVisible();

    // Side-by-side layout used instead
    await expect(page.getByRole('textbox', { name: 'Enter command...' })).toBeVisible();

    // No bottom tab bar visible (mobile tabs are hidden via CSS)
    const mobileTabBar = page.locator('nav.md\\:hidden');
    await expect(mobileTabBar).not.toBeVisible();

    const chatTab = mobileTabBar.getByRole('button', { name: 'Chat' });
    const inboxTab = mobileTabBar.getByRole('button', { name: 'Inbox' });
    await expect(chatTab).not.toBeVisible();
    await expect(inboxTab).not.toBeVisible();
  });
});
