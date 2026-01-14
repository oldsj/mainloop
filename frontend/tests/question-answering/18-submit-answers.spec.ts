// spec: frontend/tests/question-answering.plan.md
// seed: frontend/tests/fixtures/seed-data.ts

import { test, expect } from '../fixtures';
import { seedTaskWaitingQuestions } from '../fixtures/seed-data';

/**
 * QUESTION ANSWERING FLOW - Submit answers
 *
 * Skip: Interactive question submission UI was removed from inbox.
 * Questions now display as text content only.
 */

test.describe('Submitting Answers', () => {
  test.skip('Click Continue button to submit answers', async ({ appPage: page, userId }) => {
    await seedTaskWaitingQuestions(page, userId);
    await page.reload();
    await expect(page.getByText('Answer Questions')).toBeVisible({ timeout: 10000 });
  });
});
