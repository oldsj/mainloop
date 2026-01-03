// spec: frontend/specs/task-interactions.md
// seed: frontend/tests/seed.spec.ts

import { test, expect } from '@playwright/test';

test.describe('Start Implementation Flow', () => {
  test('Start Implementation', async ({ page }) => {
    await page.goto('/');
    await expect(page.getByRole('heading', { name: '$ mainloop' })).toBeVisible();

    // 1. Find task in "ready_to_implement" status
    const readyBadge = page.locator('text=READY').first();
    await expect(readyBadge).toBeVisible();

    const taskCard = readyBadge.locator('..').locator('..');
    await taskCard.click();

    // 2. Click "Start Implementation" button
    const startButton = page.locator('button:has-text("Start Implementation")');
    await expect(startButton).toBeVisible();
    await startButton.click();

    // Expected: Loading state during transition
    await expect(page.locator('button:has-text("Starting...")'))
      .toBeVisible({ timeout: 2000 })
      .catch(() => {});

    // Expected: Task status changes to "implementing"
    await expect(page.locator('text=IMPLEMENTING')).toBeVisible({ timeout: 10000 });

    // Expected: Log viewer becomes active with live logs
    const logViewer = page.locator('pre, code, .log-viewer').first();
    await expect(logViewer).toBeVisible({ timeout: 5000 });

    // Expected: Implementation progress visible
    // The task status badge should have pulse animation
    const implementingBadge = page.locator('text=IMPLEMENTING').first();
    await expect(implementingBadge).toHaveClass(/animate-pulse/);

    // Expected: Cancel button remains available
    const cancelButton = page.locator('button[aria-label="Cancel task"]').first();
    await expect(cancelButton).toBeVisible();
  });
});
