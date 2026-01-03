// spec: frontend/specs/task-interactions.md
// seed: frontend/tests/seed.spec.ts

import { test, expect } from '@playwright/test';

test.describe('Plan Review Flow', () => {
  test('Cancel Plan', async ({ page }) => {
    await page.goto('/');
    await expect(page.getByRole('heading', { name: '$ mainloop' })).toBeVisible();
    
    // 1. Review plan in "waiting_plan_review" status
    const reviewPlanBadge = page.locator('text=REVIEW PLAN').first();
    await expect(reviewPlanBadge).toBeVisible();
    
    const taskCard = reviewPlanBadge.locator('..').locator('..');
    await taskCard.click();

    // 2. Click "Cancel" button
    const cancelButton = page.locator('button:has-text("Cancel")').first();
    await expect(cancelButton).toBeVisible();
    
    // Set up dialog handler for confirmation
    page.on('dialog', dialog => dialog.accept());
    
    await cancelButton.click();

    // Expected: Confirmation dialog may appear (handled above)
    
    // Expected: Task status changes to "cancelled"
    await expect(page.locator('text=CANCELLED')).toBeVisible({ timeout: 10000 });
    
    // Expected: Task moves to failed/cancelled section
    // The task should no longer be in active tasks section
    const activeTasks = page.locator('.border-b.border-term-border').first();
    await expect(activeTasks.locator('text=CANCELLED')).toBeVisible();
    
    // Expected: Can no longer interact with task
    // Task should not be expandable anymore
    const expandedContent = page.locator('.prose-terminal');
    await expect(expandedContent).not.toBeVisible();
  });
});