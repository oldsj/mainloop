// spec: frontend/specs/task-interactions.md
// seed: frontend/tests/seed.spec.ts

import { test, expect } from '@playwright/test';

test.describe('Task Cancellation', () => {
  test('Cancel Active Task', async ({ page }) => {
    await page.goto('http://localhost:3031');
    await expect(page.getByRole('heading', { name: '$ mainloop' })).toBeVisible();
    
    // 1. Find an active task (any non-terminal status)
    const activeTask = page.locator('text=PLANNING, text=IMPLEMENTING, text=NEEDS INPUT, text=REVIEW PLAN').first();
    await expect(activeTask).toBeVisible();
    
    const taskCard = activeTask.locator('..').locator('..');

    // 2. Click the cancel button (X icon in header)
    const cancelButton = taskCard.locator('button[aria-label="Cancel task"]');
    await expect(cancelButton).toBeVisible();
    
    // Expected: Confirmation may be requested
    page.on('dialog', dialog => {
      expect(dialog.message()).toContain('Cancel this task');
      dialog.accept();
    });
    
    await cancelButton.click();

    // Expected: Task status changes to "cancelled"
    await expect(page.locator('text=CANCELLED')).toBeVisible({ timeout: 10000 });
    
    // Expected: Any running operations stop
    // Expected: Task moves to cancelled/failed section
    const cancelledBadge = page.locator('text=CANCELLED').first();
    await expect(cancelledBadge).toBeVisible();
    
    // Expected: Clear indication task was cancelled (not failed)
    await expect(cancelledBadge).toHaveClass(/text-term-fg-muted/);
    await expect(cancelledBadge).not.toHaveClass(/text-term-error/);
  });
});