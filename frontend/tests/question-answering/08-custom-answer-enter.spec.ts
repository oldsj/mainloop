// spec: frontend/tests/question-answering.plan.md
// seed: frontend/tests/fixtures/seed-data.ts

import { test, expect } from '../fixtures';
import { seedTaskWaitingQuestions } from '../fixtures/seed-data';

/**
 * QUESTION ANSWERING FLOW - Custom text answer with Enter submission
 *
 * Skip: Interactive question input UI was removed from inbox.
 * Questions now display as text content only.
 */

test.describe('Custom Text Answers', () => {
  test.skip('Type custom answer and submit with Enter', async ({ appPage: page, userId }) => {
    await seedTaskWaitingQuestions(page, userId);
    await page.reload();
    await expect(page.getByText('Answer Questions')).toBeVisible({ timeout: 10000 });
  });
});
