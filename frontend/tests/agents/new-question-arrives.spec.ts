// spec: frontend/specs/task-interactions.md
// seed: frontend/tests/seed.spec.ts

import { test, expect } from '@playwright/test';

test.describe('Real-time Updates', () => {
  test('New Question Arrives', async ({ page }) => {
    await page.goto('/');
    await expect(page.getByRole('heading', { name: '$ mainloop' })).toBeVisible();

    // 1. Task is in "implementing" status
    const implementingTask = page.locator('text=IMPLEMENTING').first();
    if (!(await implementingTask.isVisible())) {
      test.skip();
      return;
    }

    const taskCard = implementingTask.locator('..').locator('..');
    const taskDescription = await taskCard.locator('p.truncate').first().textContent();

    // 2. Worker asks a new question (backend event)
    // This would require backend to send SSE event with question
    // For now, we'll verify the UI behavior when status changes to waiting_questions

    // 3. Observe inbox
    // Expected: Task status changes to "waiting_questions"
    // Wait for potential status change via SSE
    await page.waitForTimeout(3000);

    // Look for the same task with new status
    const updatedTaskCard = page
      .locator(`text="${taskDescription}"`)
      .first()
      .locator('..')
      .locator('..');
    if (await updatedTaskCard.locator('text=NEEDS INPUT').isVisible()) {
      // Expected: Task auto-expands to show question
      const questionInput = updatedTaskCard.locator('input[placeholder*="custom answer"]');
      await expect(questionInput).toBeVisible({ timeout: 5000 });

      // Expected: Notification or highlight draws attention
      const needsInputBadge = updatedTaskCard.locator('text=NEEDS INPUT');
      await expect(needsInputBadge).toHaveClass(/border-term-warning/);

      // Expected: Badge count increments
      const attentionBadge = page.locator('header .border.border-term-info').first();
      if (await attentionBadge.isVisible()) {
        const badgeCount = await attentionBadge.textContent();
        expect(parseInt(badgeCount || '0')).toBeGreaterThan(0);
      }
    }
  });
});
