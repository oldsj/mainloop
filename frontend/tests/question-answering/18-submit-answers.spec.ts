// spec: frontend/tests/question-answering.plan.md
// seed: frontend/tests/fixtures/seed-data.ts

import { test, expect } from '../fixtures';
import { seedTaskWaitingQuestions } from '../fixtures/seed-data';

/**
 * QUESTION ANSWERING FLOW - Submit answers
 *
 * Verifies the complete submission flow:
 * - Answer all questions
 * - Continue button appears when all answered
 * - Clicking Continue submits answers
 * - Button shows loading state
 * - Task status changes after submission
 */

test.describe('Submitting Answers', () => {
  test.setTimeout(30000);

  test('Click Continue button to submit answers', async ({ appPage: page, userId }) => {
    await seedTaskWaitingQuestions(page, userId);
    await page.reload();

    // Task with questions should auto-expand showing the first question
    await expect(
      page.locator('text=Which authentication method should we use?').first()
    ).toBeVisible({ timeout: 10000 });

    // 5. Answer question 1 with 'JWT tokens' option
    const q1Option = page.locator('button:has-text("JWT tokens")').first();
    await expect(q1Option).toBeVisible();
    await q1Option.click();

    // 6. Wait for second question to appear
    await expect(page.locator('text=Should we add rate limiting?')).toBeVisible({ timeout: 5000 });

    // 7. Answer question 2 with 'Yes' option
    const q2Option = page.locator('button:has-text("Yes")').first();
    await expect(q2Option).toBeVisible();
    await q2Option.click();

    // 8. Verify Continue button is visible and enabled
    const continueButton = page.locator('button:has-text("Continue")');
    await expect(continueButton).toBeVisible({ timeout: 5000 });
    await expect(continueButton).toBeEnabled();

    // 9. Click Continue button
    await continueButton.click();

    // 10. Verify button shows loading state ('Submitting...')
    await expect(page.locator('button:has-text("Submitting")')).toBeVisible({ timeout: 2000 });

    // 11. Wait for submission to complete and task status to change
    // The task should transition away from 'NEEDS INPUT' status
    // Wait for either 'PLANNING' badge or questions UI to disappear
    await expect(page.locator('text=NEEDS INPUT').first()).not.toBeVisible({ timeout: 15000 });

    // 12. Verify task status badge changed (could be PLANNING or IMPLEMENTING)
    // At minimum, verify the NEEDS INPUT badge is gone
    const taskBadges = page
      .locator('[class*="border-term"]')
      .filter({ hasText: /PLANNING|IMPLEMENTING|PLANNING/ });
    await expect(taskBadges.first()).toBeVisible({ timeout: 5000 });
  });
});
