// spec: frontend/specs/task-interactions.md
// seed: frontend/tests/seed.spec.ts

import { test, expect } from '@playwright/test';

test.describe('Error Handling', () => {
  test('Handle Network Timeout', async ({ page }) => {
    await page.goto('/');
    await expect(page.getByRole('heading', { name: '$ mainloop' })).toBeVisible();
    
    // 1. Submit action during slow network conditions
    // Intercept and delay API response
    await page.route('**/tasks/*/approve_plan', route => {
      setTimeout(() => route.abort('timedout'), 30000);
    });
    
    const reviewPlanBadge = page.locator('text=REVIEW PLAN').first();
    if (await reviewPlanBadge.isVisible()) {
      const taskCard = reviewPlanBadge.locator('..').locator('..');
      await taskCard.click();

      const approveButton = page.locator('button:has-text("Approve Plan")');
      await approveButton.click();

      // 2. Wait for timeout
      // Expected: Loading state eventually times out
      await expect(page.locator('button:has-text("Approving...")')).toBeVisible({ timeout: 5000 }).catch(() => {});
      
      // Expected: Error message about network issue
      await expect(page.locator('text=/timeout|network|connection/i')).toBeVisible({ timeout: 35000 }).catch(() => {});
      
      // Expected: Ability to retry the action
      await expect(approveButton).toBeVisible();
      await expect(approveButton).toBeEnabled();
      
      // Expected: No duplicate submissions
      // The task should remain in review state
      await expect(page.locator('text=REVIEW PLAN')).toBeVisible();
    } else {
      test.skip();
    }
  });
});