import { test, expect, apiURL } from '../fixtures';

/**
 * Planning flow tests using seeded planning sessions.
 *
 * These tests validate the planning UI/UX without requiring
 * real GitHub access by using the test seed endpoints.
 */

test.describe('Planning Flow', () => {
  test.describe.configure({ mode: 'serial' });
  test.setTimeout(120000);

  test('can start planning and see exploration progress', async ({ appPage: page, userId }) => {
    // Ask Claude to plan something - it should recognize the planning intent
    const input = page.getByPlaceholder('Enter command...').first();

    // Ensure input is ready and interactable
    await expect(input).toBeVisible();
    await expect(input).toBeEnabled();

    // Fill and submit
    await input.fill(
      'I want to plan adding a new feature. Can you help me think through the implementation?'
    );

    // Verify message was typed
    await expect(input).toHaveValue(/I want to plan/);

    // Submit
    await page.getByRole('button', { name: 'EXEC' }).click();

    // Wait for user message to appear (confirms submission worked)
    await expect(page.getByText('I want to plan adding a new feature').first()).toBeVisible({
      timeout: 10000
    });

    // Wait for Claude's response
    const response = page.locator('.message.bg-term-bg-secondary').last();
    await expect(response).toBeVisible({ timeout: 60000 });

    // Claude should ask about the repository or provide guidance
    const responseText = await response.textContent();
    expect(responseText?.toLowerCase()).toMatch(/repository|repo|github|codebase|project|which/i);
  });

  test('planning context persists across messages', async ({ appPage: page, userId }) => {
    const input = page.getByPlaceholder('Enter command...').first();

    // First message - express planning intent
    await input.fill('I want to add user authentication to a project');
    await input.press('Enter');

    // Wait for response
    await expect(page.locator('.message.bg-term-bg-secondary').last()).toBeVisible({
      timeout: 60000
    });

    // Follow-up message - Claude should remember context
    await input.fill('The repo is at https://github.com/oldsj/testrepo');
    await input.press('Enter');

    // Wait for second response
    const messages = page.locator('.message.bg-term-bg-secondary');
    await expect(messages).toHaveCount(2, { timeout: 60000 });

    // Claude should acknowledge the repo or ask for more details
    const lastResponse = await messages.last().textContent();
    expect(lastResponse?.toLowerCase()).toMatch(/testrepo|authentication|plan|start|explore/i);
  });
});
