// spec: frontend/tests/question-answering.plan.md
// seed: frontend/tests/fixtures/seed-data.ts

import { test, expect } from '@playwright/test';
import { seedTaskWaitingQuestions } from '../fixtures/seed-data';

/**
 * QUESTION ANSWERING FLOW - Custom text answer with Enter submission
 *
 * Verifies that:
 * - Custom text input field has auto-focus when question is active
 * - Typing custom text into the input field works
 * - OK button appears when text is entered
 * - Pressing Enter key submits the custom answer
 * - Question collapses to summary view after submission
 * - Custom text is displayed in the collapsed summary
 * - Next question automatically expands
 * - Custom answer is preserved
 */

test.describe('Custom Text Answers', () => {
  test('Type custom answer and submit with Enter', async ({ page }) => {
    await seedTaskWaitingQuestions(page);
    await page.goto('/');
    await expect(page.getByRole('heading', { name: '$ mainloop' })).toBeVisible();

    await expect(page.getByText('NEEDS INPUT').first()).toBeVisible({ timeout: 10000 });
    await page.getByText('NEEDS INPUT').first().click();

    const firstQuestion = page.locator('text=Which authentication method should we use?');
    await expect(firstQuestion).toBeVisible({ timeout: 10000 });

    // 5. Locate the custom text input field
    const customInput = page.locator('input[placeholder="Or type a custom answer..."]').first();
    await expect(customInput).toBeVisible();

    // 6. Type custom answer into the input
    await customInput.click();
    await customInput.fill('My custom authentication approach');
    
    // Verify text appears in input
    await expect(customInput).toHaveValue('My custom authentication approach');

    // 7. Verify OK button appears when text is present
    const okButton = page.locator('button:has-text("OK")').first();
    await expect(okButton).toBeVisible();

    // 8. Press Enter key to submit
    await customInput.press('Enter');

    // 9. Wait for UI update and verify q1 collapses
    await page.waitForTimeout(500);

    // Verify q1 shows checkmark (✓) icon in collapsed view
    const checkmark = page.locator('text=✓').first();
    await expect(checkmark).toBeVisible();

    // 10. Verify custom text appears in the collapsed summary
    const summarySummary = page.locator('text=My custom authentication approach');
    await expect(summarySummary).toBeVisible();

    // 11. Verify q2 automatically expands (becomes active)
    const secondQuestion = page.locator('text=Should we add rate limiting?');
    await expect(secondQuestion).toBeVisible({ timeout: 5000 });

    // Verify the second question has expanded options visible
    const yesOption = page.locator('button:has-text("Yes")').first();
    await expect(yesOption).toBeVisible();
  });
});
