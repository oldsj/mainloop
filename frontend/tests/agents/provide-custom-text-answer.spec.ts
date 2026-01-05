import { test, expect } from '@playwright/test';
import { seedTaskWaitingQuestions } from '../fixtures/seed-data';

test.describe('Question Answering Flow', () => {
  test.skip('Provide Custom Text Answer', async ({ page }) => {
    await page.goto('/');
    await seedTaskWaitingQuestions(page);
    await page.reload();

    await expect(page.getByRole('heading', { name: '$ mainloop' })).toBeVisible();

    // 1. View a question in expanded state
    const needsInputBadge = page.locator('text=NEEDS INPUT').first();
    await expect(needsInputBadge).toBeVisible({ timeout: 10000 });

    const taskCard = needsInputBadge.locator('..').locator('..');
    await taskCard.click();

    // 2. Ignore the option buttons
    // 3. Type custom answer in text input field
    const customInput = page.getByPlaceholder('Or type a custom answer...');
    await expect(customInput).toBeVisible();
    await customInput.fill('My custom answer to this question');

    // 4. Press Enter or click OK
    const okButton = page.locator('button:has-text("OK")').first();
    await expect(okButton).toBeVisible();
    await okButton.click();

    // Expected: Custom text saved as the answer
    const answeredQuestion = page.locator('text=My custom answer to this question').first();
    await expect(answeredQuestion).toBeVisible();

    // Expected: Option buttons deselected if any were selected
    // Expected: Question collapses showing the custom text
    const collapsedAnswer = page.locator('button').filter({ hasText: '✓' }).first();
    await expect(collapsedAnswer).toBeVisible();
    await expect(collapsedAnswer).toContainText('My custom answer');

    // Expected: Advances to next unanswered question
    const questionNumber2 = page.locator('span').filter({ hasText: /^2$/ }).first();
    if (await questionNumber2.isVisible()) {
      const customInput2 = page.getByPlaceholder('Or type a custom answer...').first();
      await expect(customInput2).toBeVisible();
    }
  });
});
