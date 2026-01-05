import { test, expect } from '@playwright/test';
import { seedTaskWaitingPlanReview } from '../fixtures/seed-data';

test.describe('Plan Review Flow', () => {
  test('Cancel Plan', async ({ page }) => {
    // Set up dialog handler FIRST
    page.on('dialog', (dialog) => dialog.accept());

    // Seed task then navigate
    await seedTaskWaitingPlanReview(page);
    await page.goto('/');

    await expect(page.getByRole('heading', { name: '$ mainloop' })).toBeVisible();

    // Task should be visible with REVIEW PLAN status
    const reviewPlanBadge = page.locator('text=REVIEW PLAN').first();
    await expect(reviewPlanBadge).toBeVisible({ timeout: 10000 });

    // Plan content should auto-expand (don't click - that would collapse it!)
    const planContent = page.locator('.prose-terminal').first();
    await expect(planContent).toBeVisible({ timeout: 5000 });

    // Cancel button should be visible
    const cancelButton = page.locator('button:has-text("Cancel")').first();
    await expect(cancelButton).toBeVisible();

    // Click cancel
    await cancelButton.click();

    // Task should move to History section
    const historySection = page.locator('text=History').first();
    await expect(historySection).toBeVisible({ timeout: 10000 });

    // Expand History to see cancelled task
    await historySection.click();

    // Task status should be CANCELLED
    await expect(page.locator('text=CANCELLED')).toBeVisible({ timeout: 5000 });
  });
});
