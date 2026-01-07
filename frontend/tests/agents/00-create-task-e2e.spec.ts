import { test, expect, setupConversation } from '../fixtures';

/**
 * AGENTS STAGE - End-to-end task creation
 *
 * Tests the full flow: conversation → Claude spawns task → task appears in inbox
 */

test.describe.configure({ mode: 'serial' }); // Real Claude API calls must run serially

test.describe('Agents: Create Task (E2E)', () => {
  test.setTimeout(60000); // 1 min - haiku is fast

  test('create task via conversation', async ({ appPage: page }) => {
    // appPage already navigated to / and verified app is ready
    await expect(page.getByRole('heading', { name: '$ mainloop' })).toBeVisible();

    // Set up conversation state
    await setupConversation(page);

    const input = page.getByPlaceholder('Enter command...').first();

    // Request task with explicit spawn instruction
    await input.fill(
      'Spawn a task to update the README on https://github.com/oldsj/mainloop - add a quick start section. I confirm, please spawn now.'
    );
    await input.press('Enter');

    // Wait for task to appear in the sidebar projects list
    // The task repo should show up as a new project
    await expect(
      page.locator('[data-testid="projects-list"]').getByText('oldsj/mainloop')
    ).toBeVisible({
      timeout: 30000
    });
  });
});
