import { test, expect, seedTask } from '../fixtures';

test.describe('Plan Review Flow', () => {
  test('Approve Plan', async ({ appPage: page, userId }) => {
    // Seed a task in "waiting_plan_review" status
    await seedTask(page, userId, {
      status: 'waiting_plan_review',
      description: 'Add user authentication',
      plan: '# Implementation Plan\n\n## Steps\n1. Create User model\n2. Add auth endpoints'
    });

    // Refresh to pick up seeded task
    await page.reload();
    await expect(page.getByRole('heading', { name: '$ mainloop' })).toBeVisible();

    // Review plan in "waiting_plan_review" status
    const reviewPlanBadge = page.locator('text=REVIEW PLAN').first();
    await expect(reviewPlanBadge).toBeVisible();

    // Plan content is visible (tasks needing attention auto-expand on load)
    const planContent = page.locator('.prose-terminal').first();
    await expect(planContent).toBeVisible();

    // "Approve Plan" button is visible and enabled
    const approveButton = page.locator('button:has-text("Approve Plan")');
    await expect(approveButton).toBeVisible();
    await expect(approveButton).toBeEnabled();
  });
});
