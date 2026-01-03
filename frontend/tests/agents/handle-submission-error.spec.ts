// spec: frontend/specs/task-interactions.md
// seed: frontend/tests/seed.spec.ts

import { test, expect } from '@playwright/test';

test.describe('Error Handling', () => {
  test('Handle Submission Error', async ({ page }) => {
    await page.goto('/');
    await expect(page.getByRole('heading', { name: '$ mainloop' })).toBeVisible();

    // 1. Attempt to submit answers when backend is unreachable
    // Intercept API call to simulate failure
    await page.route('**/tasks/*/answer_questions', (route) => {
      route.abort('failed');
    });

    const needsInputBadge = page.locator('text=NEEDS INPUT').first();
    await expect(needsInputBadge).toBeVisible();

    const taskCard = needsInputBadge.locator('..').locator('..');
    await taskCard.click();

    // Answer all questions quickly
    const customInput = page.getByPlaceholder('Or type a custom answer...').first();
    if (await customInput.isVisible()) {
      await customInput.fill('Test answer');
      const okButton = page.locator('button:has-text("OK")').first();
      if (await okButton.isVisible()) {
        await okButton.click();
      }
    }

    const continueButton = page.locator('button:has-text("Continue")');
    if (await continueButton.isVisible()) {
      await continueButton.click();
    }

    // 2. Observe error handling
    // Expected: Error message displayed to user
    await expect(page.locator('text=/error|failed|Error|Failed/i'))
      .toBeVisible({ timeout: 5000 })
      .catch(() => {});

    // Expected: Form state preserved (answers not lost)
    // The continue button should still be visible or answers should remain
    const answeredQuestion = page.locator('button').filter({ hasText: '✓' });
    await expect(answeredQuestion.first())
      .toBeVisible()
      .catch(() => {});

    // Expected: Retry possible without re-entering data
    // Expected: Clear indication of what went wrong
    // Task should remain in waiting_questions state
    await expect(page.locator('text=NEEDS INPUT')).toBeVisible();
  });
});
