import { test, expect } from '@playwright/test';
import { setupConversation, resetTestData } from '../fixtures';

/**
 * AGENTS STAGE - Mock Claude task spawning
 *
 * Tests the spawn flow with mocked Claude:
 * conversation → mock Claude calls spawn_task → task appears in inbox
 *
 * This test runs with USE_MOCK_CLAUDE=true and validates that:
 * 1. Mock Claude detects spawn intent
 * 2. Mock Claude calls the real spawn_task tool
 * 3. Task is created in DB and appears in inbox
 */

test.describe('Agents: Spawn Task (Mocked Claude)', () => {
  test.beforeEach(async ({ request }) => {
    await resetTestData(request);
  });

  test('mock Claude spawns task when given repo URL', async ({ page }) => {
    await page.goto('/');
    await expect(page.getByRole('heading', { name: '$ mainloop' })).toBeVisible();

    // Set up conversation state
    await setupConversation(page);

    // Ask to spawn a task with a GitHub URL
    // Mock Claude should detect spawn intent and call spawn_task tool
    const input = page.getByPlaceholder('Enter command...').first();
    await input.fill('implement a dark mode feature for https://github.com/test/demo-repo');
    await input.press('Enter');

    // Message appears in chat
    await expect(page.getByText('implement a dark mode feature').first()).toBeVisible();

    // Wait for Claude response (mock should call spawn_task)
    // Mock Claude says "I've spawned a worker agent..."
    await expect(
      page
        .locator('.message')
        .filter({ hasText: /spawn|worker|agent|task/i })
        .first()
    ).toBeVisible({ timeout: 15000 });

    // Task should appear in inbox with PLANNING status
    // Wait longer since DBOS workflow needs to process
    await expect(page.getByText(/PLANNING|REVIEW/i).first()).toBeVisible({ timeout: 30000 });
  });

  test('mock Claude asks for repo URL when not provided', async ({ page }) => {
    await page.goto('/');
    await expect(page.getByRole('heading', { name: '$ mainloop' })).toBeVisible();

    await setupConversation(page);

    // Ask to spawn without a GitHub URL
    const input = page.getByPlaceholder('Enter command...').first();
    await input.fill('create a new feature for my project');
    await input.press('Enter');

    // Mock Claude should ask for the repository URL
    await expect(
      page
        .locator('.message')
        .filter({ hasText: /repository|GitHub|URL/i })
        .first()
    ).toBeVisible({ timeout: 10000 });
  });
});
