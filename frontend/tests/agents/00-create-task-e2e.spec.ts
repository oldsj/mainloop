import { test, expect } from '@playwright/test';

/**
 * AGENTS STAGE - End-to-end task creation
 *
 * Tests the full flow: conversation → Claude spawns task → task appears in inbox
 * This builds on the conversation history from previous stages.
 */

test.describe('Agents: Create Task (E2E)', () => {
  test('create task via conversation', async ({ page }) => {
    await page.goto('/');
    await expect(page.getByRole('heading', { name: '$ mainloop' })).toBeVisible();

    // Verify we have conversation history from previous stages
    await expect(page.getByText('hello').first()).toBeVisible();

    // Ask Claude to create a task (use first input - desktop)
    const input = page.getByPlaceholder('Enter command...').first();
    await input.fill(
      'can you implement a simple login feature for https://github.com/test/demo-repo?'
    );
    await input.press('Enter');

    // Message appears in chat
    await expect(page.getByText('can you implement a simple login feature')).toBeVisible();

    // Claude should ask for confirmation before spawning
    // Wait for response that includes spawn confirmation or question
    await expect(
      page
        .locator('.message')
        .filter({ hasText: /spawn|create|implement|task/i })
        .last()
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
