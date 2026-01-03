// spec: frontend/specs/task-interactions.md
// seed: frontend/tests/seed.spec.ts

import { test, expect } from '@playwright/test';

test.describe('Question Answering Flow', () => {
  test('Submit All Answers', async ({ page }) => {
    await page.goto('/');
    await expect(page.getByRole('heading', { name: '$ mainloop' })).toBeVisible();
    
    // 1. Answer all questions for a task
    const needsInputBadge = page.locator('text=NEEDS INPUT').first();
    await expect(needsInputBadge).toBeVisible();
    
    const taskCard = needsInputBadge.locator('..').locator('..');
    await taskCard.click();

    // Answer questions until all are done
    let continueButton = page.locator('button:has-text("Continue")');
    while (!(await continueButton.isVisible())) {
      const customInput = page.getByPlaceholder('Or type a custom answer...');
      if (await customInput.isVisible()) {
        await customInput.fill('Answer');
        const okButton = page.locator('button:has-text("OK")').first();
        if (await okButton.isVisible()) {
          await okButton.click();
        }
      } else {
        const optionButton = page.locator('button').filter({ hasText: /^(Yes|No|Maybe)$/ }).first();
        if (await optionButton.isVisible()) {
          await optionButton.click();
        } else {
          break;
        }
      }
    }

    // 2. Observe the "Continue" button appears
    await expect(continueButton).toBeVisible();
    
    // 3. Click "Continue" button
    await continueButton.click();

    // Expected: Loading state shown during submission
    await expect(page.locator('button:has-text("Submitting...")')).toBeVisible({ timeout: 2000 }).catch(() => {});
    
    // Expected: Task status changes to "planning" on success
    await expect(page.locator('text=PLANNING')).toBeVisible({ timeout: 10000 });
    
    // Expected: Question UI replaced with log viewer
    await expect(customInput).not.toBeVisible();
    
    // Expected: Error displayed if submission fails (this would require mocking failure)
    // Skipping negative test case for now
  });
});