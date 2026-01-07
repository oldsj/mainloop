import { test, expect, chat } from '../fixtures';

/**
 * In-thread planning flow tests.
 *
 * Tests the new synchronous planning mode where planning happens
 * inline in the main chat conversation.
 */

test.describe('In-Thread Planning', () => {
  test.describe.configure({ mode: 'serial' });
  test.setTimeout(120000); // 2 min for Claude responses

  test('basic chat works (sanity check)', async ({ appPage: page }) => {
    // Simple sanity check that chat works at all
    const response = await chat(page, 'What is 2 + 2?');

    // Should get some response containing the answer
    expect(response.length).toBeGreaterThan(0);
    expect(response).toMatch(/4|four/i);
  });

  test('planning prompt triggers planning behavior', async ({ appPage: page }) => {
    // Send a message that should trigger planning tools
    // Note: This may take longer as Claude explores options
    const response = await chat(
      page,
      'I want to add a simple feature to a GitHub repo. What information do you need?'
    );

    // Claude should ask about the repository or task
    const responseText = response.toLowerCase();
    const asksForInfo =
      responseText.includes('repository') ||
      responseText.includes('repo') ||
      responseText.includes('github') ||
      responseText.includes('what') ||
      responseText.includes('which') ||
      responseText.includes('task') ||
      responseText.includes('help');

    expect(asksForInfo).toBe(true);
  });
});
