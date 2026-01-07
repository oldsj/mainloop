import { test, expect, seedTask } from '../fixtures';

test.describe('Plan Review Flow', () => {
  test('Cancel Plan', async ({ appPage: page, userId }) => {
    // Set up dialog handler
    page.on('dialog', (dialog) => dialog.accept());

    // Seed a task in "waiting_plan_review" status
    await seedTask(page, userId, {
      status: 'waiting_plan_review',
      description: 'Add user authentication',
      plan: '# Implementation Plan\n\n## Steps\n1. Create User model\n2. Add auth endpoints'
    });

    // Refresh to pick up seeded task
    await page.reload();
    await expect(page.getByRole('heading', { name: '$ mainloop' })).toBeVisible();

    // Task should be visible with REVIEW PLAN status
    const reviewPlanBadge = page.locator('text=REVIEW PLAN').first();
    await expect(reviewPlanBadge).toBeVisible({ timeout: 10000 });

    // Plan content should auto-expand
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
