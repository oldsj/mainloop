// spec: frontend/specs/inbox-management.md
// seed: frontend/tests/seed.spec.ts

import { test, expect } from '@playwright/test';

test.describe('Empty States', () => {
  test('Empty Inbox', async ({ page }) => {
    // 1. Load application to observe empty inbox state
    await page.goto('http://localhost:3031');

    // 2. Wait for app to fully load
    await expect(page.getByRole('heading', { name: '$ mainloop' })).toBeVisible();

    // Verify inbox panel is visible
    await expect(page.getByRole('heading', { name: '[INBOX]' })).toBeVisible();

    // Verify empty state shows terminal-style command prompt
    await expect(page.getByText('$ ls inbox/')).toBeVisible();

    // Verify empty state message 'All caught up' is displayed
    await expect(page.getByText('All caught up')).toBeVisible();
  });
});
