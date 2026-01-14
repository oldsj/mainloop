// spec: frontend/tests/question-answering.plan.md
// seed: frontend/tests/fixtures/seed-data.ts

import { test, expect } from '../fixtures';
import { seedTaskWaitingQuestions } from '../fixtures/seed-data';

/**
 * QUESTION ANSWERING FLOW - Display task with NEEDS INPUT badge
 *
 * NOTE: The inbox UI was simplified. Questions now show as queue items
 * with title "Answer Questions" and raw content. Interactive option
 * buttons were removed.
 */

test.describe('Question Viewing and Display', () => {
  test('Display task with NEEDS INPUT badge', async ({ appPage: page, userId }) => {
    // Seed and reload to pick up the new task
    await seedTaskWaitingQuestions(page, userId);
    await page.reload();

    // Question item appears in inbox with title
    await expect(page.getByText('Answer Questions')).toBeVisible({ timeout: 10000 });

    // Question content is visible (shown as text)
    await expect(page.getByText('Which authentication method')).toBeVisible();
  });
});
