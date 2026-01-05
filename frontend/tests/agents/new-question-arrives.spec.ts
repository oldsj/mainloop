import { test, expect } from '@playwright/test';
import { seedTaskWaitingQuestions } from '../fixtures/seed-data';

test.describe('Real-time Updates', () => {
  test.skip('Task with questions shows NEEDS INPUT status', async ({ page }) => {
    // Seed a task in waiting_questions state
    await page.goto('/');
    await seedTaskWaitingQuestions(page);
    await page.reload();

    await expect(page.getByRole('heading', { name: '$ mainloop' })).toBeVisible();

    // Task should appear with NEEDS INPUT status
    const needsInputBadge = page.locator('text=NEEDS INPUT').first();
    await expect(needsInputBadge).toBeVisible({ timeout: 10000 });

    // Verify the task description
    await expect(page.locator('text=Add user authentication').first()).toBeVisible();

    // Verify questions are visible when task is expanded
    // Click on the task to expand it if needed
    const taskCard = needsInputBadge
      .locator('xpath=ancestor::div[contains(@class, "border")]')
      .first();
    await taskCard.click();

    // Question should be visible
    await expect(page.locator('text=Which authentication method').first()).toBeVisible({
      timeout: 5000
    });

    // Options should be visible
    await expect(page.locator('text=Yes').first()).toBeVisible();
    await expect(page.locator('text=No').first()).toBeVisible();
  });

  test.skip('New question arrives via SSE', async () => {
    // TODO: This test requires SSE event mocking or backend trigger
    // To properly test:
    // 1. Seed task in implementing state
    // 2. Trigger backend to add a new question
    // 3. Verify question appears without reload
  });
});
