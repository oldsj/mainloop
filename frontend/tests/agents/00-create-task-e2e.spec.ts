import { test, expect } from '@playwright/test';
import { setupConversation } from '../fixtures';

/**
 * AGENTS STAGE - End-to-end task creation
 *
 * Tests the full flow: conversation → Claude spawns task → task appears in inbox
 */

test.describe('Agents: Create Task (E2E)', () => {
  // This test requires real Claude - skip unless explicitly enabled
  test.skip(!process.env.RUN_REAL_CLAUDE_TESTS, 'Skipping: requires real Claude (set RUN_REAL_CLAUDE_TESTS=1)');

  test('create task via conversation', async ({ page }) => {
    await page.goto('/');
    await expect(page.getByRole('heading', { name: '$ mainloop' })).toBeVisible();

    // Set up conversation state
    await setupConversation(page);

    // Ask Claude to create a task (use first input - desktop)
    const input = page.getByPlaceholder('Enter command...').first();
    await input.fill(
      'can you implement a simple login feature for https://github.com/test/demo-repo?'
    );
    await input.press('Enter');

    // Message appears in chat (use .first() since DB may have history from previous runs)
    await expect(page.getByText('can you implement a simple login feature').first()).toBeVisible();

    // Claude should ask for confirmation before spawning
    // Wait for response that includes spawn confirmation or question
    // Use getByRole('main') to scope to desktop layout (mobile duplicates are hidden)
    await expect(
      page
        .getByRole('main')
        .locator('.message')
        .filter({ hasText: /spawn|create|implement|task/i })
        .first()
    ).toBeVisible({ timeout: 30000 });

    // If Claude asks for confirmation, look for a confirm button or message
    // This is flexible - Claude might ask different ways
    const hasConfirmButton = await page
      .locator('button:has-text("Confirm")')
      .isVisible()
      .catch(() => false);

    if (hasConfirmButton) {
      await page.locator('button:has-text("Confirm")').click();
    } else {
      // Claude might just ask yes/no in text, reply yes
      const replyInput = page.getByPlaceholder('Enter command...').first();
      await replyInput.fill('yes, please proceed');
      await replyInput.press('Enter');
    }

    // Wait for task to appear in inbox
    // Task should show in "PLANNING" or "REVIEW PLAN" state
    await expect(page.locator('text=PLANNING, text=REVIEW PLAN').first()).toBeVisible({
      timeout: 60000
    });

    // Verify task appears in inbox with description
    await expect(page.locator('text=login feature, text=authentication').first()).toBeVisible();
  });
});
