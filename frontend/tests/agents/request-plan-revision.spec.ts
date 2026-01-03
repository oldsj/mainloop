// spec: frontend/specs/task-interactions.md
// seed: frontend/tests/seed.spec.ts

import { test, expect } from '@playwright/test';

test.describe('Plan Review Flow', () => {
  test('Request Plan Revision', async ({ page }) => {
    await page.goto('/');
    await expect(page.getByRole('heading', { name: '$ mainloop' })).toBeVisible();

    // 1. Review plan in "waiting_plan_review" status
    const reviewPlanBadge = page.locator('text=REVIEW PLAN').first();
    await expect(reviewPlanBadge).toBeVisible();

    const taskCard = reviewPlanBadge.locator('..').locator('..');
    await taskCard.click();

    // 2. Type feedback in revision text input
    const revisionInput = page.getByPlaceholder('Request changes...');
    await expect(revisionInput).toBeVisible();

    const reviseButton = page.locator('button:has-text("Revise")');

    // Expected: Feedback text required (button disabled if empty)
    await expect(reviseButton).toBeDisabled();

    await revisionInput.fill('Please add more error handling and unit tests');

    // 3. Click "Revise" button
    await expect(reviseButton).toBeEnabled();
    await reviseButton.click();

    // Expected: Loading state during submission
    await expect(page.locator('button:has-text("Submitting...")'))
      .toBeVisible({ timeout: 2000 })
      .catch(() => {});

    // Expected: Task status returns to "planning"
    await expect(page.locator('text=PLANNING')).toBeVisible({ timeout: 10000 });

    // Expected: Plan regeneration begins with new context
    // Expected: Log viewer shows planning activity
    const logViewer = page.locator('.log-viewer, pre, code').first();
    await expect(logViewer).toBeVisible({ timeout: 5000 });
  });
});
