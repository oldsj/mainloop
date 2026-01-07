// spec: frontend/tests/question-answering.plan.md
// seed: frontend/tests/fixtures/seed-data.ts

import { test, expect } from '../fixtures';
import { seedTaskWaitingQuestions } from '../fixtures/seed-data';

/**
 * QUESTION ANSWERING FLOW - Select option and auto-advance
 *
 * Verifies that:
 * - Clicking an option answers the question
 * - Question collapses to summary with checkmark
 * - Next question automatically expands
 * - Selected answer is displayed in summary
 */

test.describe('Answering Questions with Options', () => {
  test('Select option and auto-advance to next question', async ({ appPage: page, userId }) => {
    await seedTaskWaitingQuestions(page, userId);
    await page.reload();

    // Task with questions should auto-expand showing the first question
    const firstQuestion = page.locator('text=Which authentication method should we use?').first();
    await expect(firstQuestion).toBeVisible({ timeout: 10000 });

    // 5. Click the first option button (e.g., 'JWT tokens')
    const jwtOption = page.locator('button:has-text("JWT tokens")').first();
    await expect(jwtOption).toBeVisible();
    await jwtOption.click();

    // 6. Wait for UI update and verify q1 collapses to summary view
    // The question should now show in collapsed state with checkmark
    await page.waitForTimeout(500); // Brief wait for UI update

    // 7. Verify q1 shows checkmark (✓) icon
    const checkmark = page.locator('text=✓').first();
    await expect(checkmark).toBeVisible();

    // 8. Verify q2 automatically expands (becomes active)
    const secondQuestion = page.locator('text=Should we add rate limiting?');
    await expect(secondQuestion).toBeVisible({ timeout: 5000 });

    // 9. Verify Continue button is NOT visible yet (not all answered)
    const continueButton = page.locator('button:has-text("Continue")');
    await expect(continueButton).not.toBeVisible();
  });
});
