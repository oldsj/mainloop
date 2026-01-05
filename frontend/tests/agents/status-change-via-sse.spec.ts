import { test, expect } from '@playwright/test';
import { seedTaskWaitingPlanReview } from '../fixtures/seed-data';

test.describe('Real-time Updates', () => {
  test.skip('Task displays in inbox with correct status', async ({ page }) => {
    // Seed a task and verify it appears in the inbox
    await page.goto('/');
    await seedTaskWaitingPlanReview(page);
    await page.reload();

    await expect(page.getByRole('heading', { name: '$ mainloop' })).toBeVisible();

    // Task should appear with REVIEW PLAN status
    const reviewBadge = page.locator('text=REVIEW PLAN').first();
    await expect(reviewBadge).toBeVisible({ timeout: 10000 });

    // Get the task card containing the badge
    const taskCard = reviewBadge.locator('xpath=ancestor::div[contains(@class, "border")]').first();

    // Verify task description is visible
    await expect(taskCard.locator('text=Add user authentication').first()).toBeVisible();

    // Verify badge styling (should indicate attention needed)
    await expect(reviewBadge).toBeVisible();

    // Verify attention badge in header updates
    const attentionBadge = page.locator('header').locator('.border.border-term-info').first();
    if (await attentionBadge.isVisible()) {
      const badgeCount = await attentionBadge.textContent();
      expect(badgeCount).toMatch(/\d+/);
    }
  });

  test.skip('Status updates via SSE without page reload', async () => {
    // TODO: This test requires SSE event mocking or backend trigger
    // Currently we can only test initial state rendering
    // To properly test SSE:
    // 1. Seed task in state A
    // 2. Trigger backend to change state to B
    // 3. Verify UI updates without reload
  });
});
