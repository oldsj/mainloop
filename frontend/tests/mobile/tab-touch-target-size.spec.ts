// spec: frontend/specs/mobile-navigation.md
// seed: tests/seed.spec.ts

import { test, expect } from '@playwright/test';

test.describe('Touch Interactions', () => {
  test('Tab Touch Target Size', async ({ page }) => {
    // Set mobile viewport (Pixel 5)
    await page.setViewportSize({ width: 393, height: 851 });

    // 1. On mobile viewport with touch device
    await page.goto('http://localhost:3031');

    // Wait for page to load
    await expect(page.getByRole('heading', { name: '$ mainloop' })).toBeVisible();

    // 2. Attempt to tap each tab
    const chatTab = page.getByRole('button', { name: 'Chat' });
    const inboxTab = page.getByRole('button', { name: 'Inbox' });
    
    // Expected: Tabs have adequate touch target size (minimum 44px)
    const chatBox = await chatTab.boundingBox();
    const inboxBox = await inboxTab.boundingBox();
    
    expect(chatBox).not.toBeNull();
    expect(inboxBox).not.toBeNull();
    
    if (chatBox && inboxBox) {
      expect(chatBox.height).toBeGreaterThanOrEqual(44);
      expect(inboxBox.height).toBeGreaterThanOrEqual(44);
    }
    
    // Expected: Easy to tap without precision - tabs are clickable
    await expect(chatTab).toBeEnabled();
    await expect(inboxTab).toBeEnabled();
    
    // Expected: Clear feedback on tap (visual state change)
    await inboxTab.click();
    await expect(inboxTab).toHaveClass(/text-term-accent/);
    
    await chatTab.click();
    await expect(chatTab).toHaveClass(/text-term-accent/);
  });
});
