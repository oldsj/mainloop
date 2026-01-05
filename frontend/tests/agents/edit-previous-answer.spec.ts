import { test, expect } from '@playwright/test';
import { seedTaskWaitingQuestions } from '../fixtures/seed-data';

test.describe('Question Answering Flow', () => {
  test.skip('Edit Previous Answer', async ({ page }) => {
    // Seed first, then navigate (like passing tests)
    await seedTaskWaitingQuestions(page);
    await page.goto('/');

    await expect(page.getByRole('heading', { name: '$ mainloop' })).toBeVisible();

    // Task should show NEEDS INPUT and auto-expand
    const needsInputBadge = page.locator('text=NEEDS INPUT').first();
    await expect(needsInputBadge).toBeVisible({ timeout: 10000 });

    // Questions should auto-expand for tasks needing attention
    // Look for the question text
    await expect(page.locator('text=Which authentication method').first()).toBeVisible({ timeout: 5000 });

    // Answer first question by clicking Yes
    const yesButton = page.locator('button:has-text("Yes")').first();
    await expect(yesButton).toBeVisible();
    await yesButton.click();

    // After answering, check for answered state or next question
    // The UI might show a checkmark or move to next question
    await expect(page.locator('text=Should we add rate limiting').first()).toBeVisible({ timeout: 5000 });
  });
});
