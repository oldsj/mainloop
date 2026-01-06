// spec: frontend/specs/mobile-navigation.md
// seed: tests/seed.spec.ts

import { test, expect } from '@playwright/test';

test.describe('Tab Navigation', () => {
  test('Return to Chat Tab', async ({ page }) => {
    // Set mobile viewport (Pixel 5)
    await page.setViewportSize({ width: 393, height: 851 });

    await page.goto('/');

    // Wait for page to load
    await expect(page.getByRole('heading', { name: '$ mainloop' }).first()).toBeVisible();

    const inboxTab = page.getByTestId('tab-inbox');
    const chatTab = page.getByTestId('tab-chat');

    // Navigate to [INBOX] tab
    await inboxTab.click();
    await page.waitForTimeout(100); // Wait for Svelte reactivity
    await expect(inboxTab).toHaveClass(/text-term-accent/, { timeout: 2000 });

    // Return to [CHAT] tab
    await chatTab.click();
    await page.waitForTimeout(100); // Wait for Svelte reactivity
    await expect(chatTab).toHaveClass(/text-term-accent/, { timeout: 2000 });
    await expect(inboxTab).not.toHaveClass(/text-term-accent/);
  });
});
