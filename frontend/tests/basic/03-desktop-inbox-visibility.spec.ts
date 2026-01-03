// spec: frontend/specs/inbox-management.md
// seed: frontend/tests/seed.spec.ts

import { test, expect } from '@playwright/test';

test.describe('Inbox Panel Visibility', () => {
  test('Desktop Inbox Always Visible', async ({ page }) => {
    // 1. Load application at desktop viewport (1280x720)
    await page.goto('/');

    // 2. Wait for app to fully load (header shows "$ mainloop")
    await expect(page.getByRole('heading', { name: '$ mainloop' })).toBeVisible();

    // Verify inbox panel header shows "[INBOX]" with terminal styling
    await expect(page.getByRole('heading', { name: '[INBOX]' })).toBeVisible();

    // Verify project filter dropdown visible in inbox header
    await expect(page.getByRole('button', { name: 'All Projects' })).toBeVisible();

    // Verify inbox panel is visible on right side showing empty state
    await expect(page.getByText('All caught up')).toBeVisible();
  });
});
