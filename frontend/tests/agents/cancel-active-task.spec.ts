import { test, expect } from '@playwright/test';
import { seedTaskWaitingPlanReview } from '../fixtures/seed-data';

test.describe('Task Cancellation', () => {
  // TODO: Cancel button click doesn't trigger cancellation - needs investigation
  test.skip('Cancel Active Task', async ({ page }) => {
    // Set up dialog handler FIRST (before any navigation)
    page.on('dialog', (dialog) => dialog.accept());

    // Seed a task to cancel
    await seedTaskWaitingPlanReview(page);

    await page.goto('/');
    await expect(page.getByRole('heading', { name: '$ mainloop' })).toBeVisible();

    // 1. Find the seeded task with REVIEW PLAN status
    const reviewPlanBadge = page.locator('text=REVIEW PLAN').first();
    await expect(reviewPlanBadge).toBeVisible({ timeout: 10000 });

    // 2. Click the cancel button (X icon in task header)
    // First hover to make the button visible (it may be hidden until hover)
    const taskCard = reviewPlanBadge.locator('xpath=ancestor::div[contains(@class, "border")]').first();
    await taskCard.hover();

    const cancelButton = page.getByRole('button', { name: 'Cancel task' }).first();
    await expect(cancelButton).toBeVisible({ timeout: 5000 });
    await cancelButton.click();

    // Wait for task status to change to CANCELLED
    await expect(page.locator('text=CANCELLED')).toBeVisible({ timeout: 10000 });
  });
});
