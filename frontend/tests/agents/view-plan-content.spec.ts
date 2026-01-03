// spec: frontend/specs/task-interactions.md
// seed: frontend/tests/seed.spec.ts

import { test, expect } from '@playwright/test';

test.describe('Plan Review Flow', () => {
  test('View Plan Content', async ({ page }) => {
    await page.goto('/');
    await expect(page.getByRole('heading', { name: '$ mainloop' })).toBeVisible();
    
    // 1. Expand a task in "waiting_plan_review" status
    const reviewPlanBadge = page.locator('text=REVIEW PLAN').first();
    await expect(reviewPlanBadge).toBeVisible();
    
    const taskCard = reviewPlanBadge.locator('..').locator('..');
    await taskCard.click();

    // 2. Observe the plan display
    // Expected: Plan rendered with full markdown formatting
    const planContent = page.locator('.prose-terminal').first();
    await expect(planContent).toBeVisible();
    
    // Expected: Code blocks styled appropriately
    // (Would need actual markdown content to test this)
    
    // Expected: Scrollable container for long plans
    await expect(planContent.locator('..')).toHaveCSS('overflow-y', 'auto');
    
    // Expected: "Approve Plan" button visible (green styling)
    const approveButton = page.locator('button:has-text("Approve Plan")');
    await expect(approveButton).toBeVisible();
    await expect(approveButton).toHaveClass(/border-term-accent-alt/);
    await expect(approveButton).toHaveClass(/text-term-accent-alt/);
    
    // Expected: "Cancel" button visible
    const cancelButton = page.locator('button:has-text("Cancel")');
    await expect(cancelButton).toBeVisible();
    
    // Expected: Revision text input field available
    const revisionInput = page.getByPlaceholder('Request changes...');
    await expect(revisionInput).toBeVisible();
  });
});