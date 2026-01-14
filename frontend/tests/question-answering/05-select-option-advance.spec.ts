// spec: frontend/tests/question-answering.plan.md
// seed: frontend/tests/fixtures/seed-data.ts

import { test, expect } from '../fixtures';
import { seedTaskWaitingQuestions } from '../fixtures/seed-data';

/**
 * QUESTION ANSWERING FLOW - Select option and auto-advance
 *
 * Skip: Interactive option buttons were removed from inbox UI.
 * Questions now display as text content only.
 */

test.describe('Answering Questions with Options', () => {
  test.skip('Select option and auto-advance to next question', async ({
    appPage: page,
    userId
  }) => {
    await seedTaskWaitingQuestions(page, userId);
    await page.reload();
    await expect(page.getByText('Answer Questions')).toBeVisible({ timeout: 10000 });
  });
});
