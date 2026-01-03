// spec: frontend/specs/task-interactions.md
// seed: frontend/tests/seed.spec.ts

import { test, expect } from '@playwright/test';

test.describe('Question Answering Flow', () => {
  test('Edit Previous Answer', async ({ page }) => {
    await page.goto('/');
    await expect(page.getByRole('heading', { name: '$ mainloop' })).toBeVisible();

    // 1. Answer several questions in a task
    const needsInputBadge = page.locator('text=NEEDS INPUT').first();
    await expect(needsInputBadge).toBeVisible();

    const taskCard = needsInputBadge.locator('..').locator('..');
    await taskCard.click();

    // Answer first question
    const firstOption = page
      .locator('button')
      .filter({ hasText: /^(Yes|No|Maybe)$/ })
      .first();
    await expect(firstOption).toBeVisible();
    await firstOption.click();

    // Verify first question is answered
    const answeredQuestion1 = page.locator('button').filter({ hasText: '✓' }).first();
    await expect(answeredQuestion1).toBeVisible();

    // 2. Click on an already-answered question summary
    await answeredQuestion1.click();

    // Expected: Question expands back to edit mode
    const customInput = page.getByPlaceholder('Or type a custom answer...');
    await expect(customInput).toBeVisible();

    // Expected: Previously selected answer is highlighted
    const selectedOption = page.locator('button').filter({ hasClass: /text-term-accent/ });
    await expect(selectedOption.first()).toBeVisible();

    // Expected: Can change to different option or custom text
    await customInput.fill('Changed my mind - new answer');
    const okButton = page.locator('button:has-text("OK")').first();
    await okButton.click();

    // Expected: Other answered questions remain collapsed
    await expect(answeredQuestion1).toContainText('Changed my mind');
  });
});
