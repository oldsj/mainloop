// spec: frontend/specs/task-interactions.md
// seed: frontend/tests/seed.spec.ts

import { test, expect } from '@playwright/test';

test.describe('Question Answering Flow', () => {
  test('Select Option Answer', async ({ page }) => {
    await page.goto('/');
    await expect(page.getByRole('heading', { name: '$ mainloop' })).toBeVisible();

    // 1. View a question with multiple options
    const needsInputBadge = page.locator('text=NEEDS INPUT').first();
    await expect(needsInputBadge).toBeVisible();

    const taskCard = needsInputBadge.locator('..').locator('..');
    await taskCard.click();

    // Find the first option button
    const firstOption = page
      .locator('button')
      .filter({ hasText: /^(Yes|No|Maybe)$/ })
      .first();
    await expect(firstOption).toBeVisible();

    // 2. Click one of the option buttons
    await firstOption.click();

    // Expected: Selected option highlighted with accent color (term-accent)
    await expect(firstOption).toHaveClass(/text-term-accent/);
    await expect(firstOption).toHaveClass(/border-term-accent/);

    // Expected: Question collapses to show answer summary
    const answeredQuestion = page.locator('button').filter({ hasText: '✓' }).first();
    await expect(answeredQuestion).toBeVisible();

    // Expected: Next unanswered question auto-expands (if exists)
    const questionNumber2 = page.locator('span').filter({ hasText: /^2$/ }).first();
    if (await questionNumber2.isVisible()) {
      const activeQuestion = questionNumber2.locator('..').locator('..');
      await expect(activeQuestion).toHaveClass(/border-term-accent/);
    }

    // Expected: Progress indicator updates
    await expect(answeredQuestion.locator('span:has-text("✓")')).toBeVisible();
  });
});
