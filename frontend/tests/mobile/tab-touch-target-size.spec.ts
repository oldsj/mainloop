import { test, expect } from '../fixtures';

test.describe('Touch Interactions', () => {
  test('Tab Touch Target Size', async ({ appPage: page }) => {
    const chatTab = page.getByRole('button', { name: 'Chat' });
    const inboxTab = page.getByRole('button', { name: 'Inbox' });

    // Tabs have adequate touch target size (minimum 44px)
    const chatBox = await chatTab.boundingBox();
    const inboxBox = await inboxTab.boundingBox();

    expect(chatBox).not.toBeNull();
    expect(inboxBox).not.toBeNull();

    if (chatBox && inboxBox) {
      expect(chatBox.height).toBeGreaterThanOrEqual(44);
      expect(inboxBox.height).toBeGreaterThanOrEqual(44);
    }

    // Tabs are clickable
    await expect(chatTab).toBeEnabled();
    await expect(inboxTab).toBeEnabled();
    await expect(chatTab).toBeVisible();
    await expect(inboxTab).toBeVisible();
  });
});
