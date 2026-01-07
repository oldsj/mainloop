// spec: frontend/tests/question-answering.plan.md
// seed: frontend/tests/fixtures/seed-data.ts

import { test, expect } from '../fixtures';
import { seedTaskWaitingQuestions } from '../fixtures/seed-data';

/**
 * QUESTION ANSWERING FLOW - Display task with NEEDS INPUT badge
 *
 * Verifies that tasks in waiting_questions status:
 * - Display with NEEDS INPUT badge
 * - Badge has warning styling (yellow/warning color)
 * - Task is automatically expanded
 * - First question is visible and expanded
 */

test.describe('Question Viewing and Display', () => {
  test('Display task with NEEDS INPUT badge', async ({ appPage: page, userId }) => {
    // Seed and reload to pick up the new task
    await seedTaskWaitingQuestions(page, userId);
    await page.reload();

    // 4. Verify task appears in inbox with 'NEEDS INPUT' badge
    const needsInputBadge = page.locator('text=NEEDS INPUT').first();
    await expect(needsInputBadge).toBeVisible({ timeout: 10000 });

    // 5. Verify badge has warning styling (border-term-warning text-term-warning)
    const badgeElement = page
      .locator('.border-term-warning.text-term-warning', {
        hasText: 'NEEDS INPUT'
      })
      .first();
    await expect(badgeElement).toBeVisible();

    // 6. Verify task is auto-expanded - first question visible without clicking
    const firstQuestion = page.locator('text=Which authentication method should we use?').first();
    await expect(firstQuestion).toBeVisible({ timeout: 5000 });

    // Verify first question is in expanded state (options are visible)
    const jwtOption = page.locator('button:has-text("JWT tokens")').first();
    await expect(jwtOption).toBeVisible();

    const sessionOption = page.locator('button:has-text("Session cookies")').first();
    await expect(sessionOption).toBeVisible();

    const oauthOption = page.locator('button:has-text("OAuth 2.0")').first();
    await expect(oauthOption).toBeVisible();
  });
});
