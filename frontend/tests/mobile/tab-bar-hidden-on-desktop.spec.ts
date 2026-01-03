// spec: frontend/specs/mobile-navigation.md
// seed: tests/seed.spec.ts

import { test, expect } from '@playwright/test';

test.describe('Tab Bar Display', () => {
  test('Tab Bar Hidden on Desktop', async ({ page }) => {
    // Set desktop viewport
    await page.setViewportSize({ width: 1280, height: 720 });

    // 1. Load application at desktop viewport (1280x720)
    await page.goto('/');

    // 2. Observe bottom of screen
    // Expected: Inbox panel always visible on desktop
    await expect(page.getByRole('heading', { name: '[INBOX]' })).toBeVisible();

    // Expected: Side-by-side layout used instead
    await expect(page.getByRole('textbox', { name: 'Enter command...' })).toBeVisible();

    // Expected: No bottom tab bar visible (mobile tabs are hidden via CSS)
    // Mobile tab buttons have exact aria-labels "Chat" and "Inbox" and are inside the mobile nav
    const mobileTabBar = page.locator('nav.md\\:hidden');
    await expect(mobileTabBar).not.toBeVisible();

    const chatTab = mobileTabBar.getByRole('button', { name: 'Chat' });
    const inboxTab = mobileTabBar.getByRole('button', { name: 'Inbox' });
    await expect(chatTab).not.toBeVisible();
    await expect(inboxTab).not.toBeVisible();
  });
});
