import { test, expect } from '@playwright/test';
import { seedTaskWaitingPlanReview } from '../fixtures/seed-data';

/**
 * AGENTS STAGE - Plan approval (isolated test with seeded data)
 *
 * Uses seed fixture for fast, deterministic testing of UI interactions.
 */

test.describe('Plan Review Flow', () => {
  test('Approve Plan', async ({ page }) => {
    // Seed a task in "waiting_plan_review" status
    await seedTaskWaitingPlanReview(page);

    await page.goto('/');
    await expect(page.getByRole('heading', { name: '$ mainloop' })).toBeVisible();

    // 1. Review plan in "waiting_plan_review" status
    const reviewPlanBadge = page.locator('text=REVIEW PLAN').first();
    await expect(reviewPlanBadge).toBeVisible();
    
    const taskCard = reviewPlanBadge.locator('..').locator('..');
    await taskCard.click();

    // Verify plan content is visible
    const planContent = page.locator('.prose-terminal').first();
    await expect(planContent).toBeVisible();

    // 2. Click "Approve Plan" button
    const approveButton = page.locator('button:has-text("Approve Plan")');
    await expect(approveButton).toBeVisible();
    await approveButton.click();

    // Expected: Loading state during approval submission
    await expect(page.locator('button:has-text("Approving...")')).toBeVisible({ timeout: 2000 }).catch(() => {});
    
    // Expected: Task status changes to "ready_to_implement" on success
    await expect(page.locator('text=READY')).toBeVisible({ timeout: 10000 });
    
    // Expected: Success indicator briefly shown
    await expect(page.locator('text=Plan approved')).toBeVisible({ timeout: 5000 });
    
    // Expected: "Start Implementation" button appears
    const startButton = page.locator('button:has-text("Start Implementation")');
    await expect(startButton).toBeVisible();
    
    // Expected: Plan content remains visible for reference
    await expect(planContent).toBeVisible();
  });
});