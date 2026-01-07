import { test, expect, setupConversation } from '../fixtures';

/**
 * CONTEXT STAGE - Verify conversation history
 *
 * Self-contained tests for conversation persistence.
 */

test.describe.configure({ mode: 'serial' }); // Real Claude API calls must run serially

test.describe('Context: Conversation History', () => {
  test('messages are visible after sending', async ({ appPage: page }) => {
    // appPage already navigated to / and verified app is ready

    // Set up conversation (self-contained)
    await setupConversation(page);

    // The "hello" message should be visible
    await expect(page.getByText('hello').first()).toBeVisible();

    // There should be messages in the conversation
    const messages = page.locator('.message');
    expect(await messages.count()).toBeGreaterThan(0);
  });

  test('send another message maintains history', async ({ appPage: page }) => {
    // appPage already navigated to / and verified app is ready

    // Set up initial conversation (self-contained)
    await setupConversation(page);

    // Count messages before sending (scope to main to avoid counting mobile layout)
    const messages = page.getByRole('main').locator('.message');
    const countBefore = await messages.count();

    // Send a new message (use first input - desktop)
    const input = page.getByPlaceholder('Enter command...').first();
    await input.fill('what did I just say?');
    await input.press('Enter');

    // New message appears
    await expect(page.getByText('what did I just say?').first()).toBeVisible();

    // Wait for loading to complete (response received)
    // Use getByRole('main') to target only the desktop layout (mobile is hidden but still in DOM)
    await expect(page.getByRole('main').getByText('processing')).toBeHidden({ timeout: 60000 });

    // Verify response arrived (at least 2 new messages: user + assistant)
    await expect(messages).toHaveCount(countBefore + 2, { timeout: 5000 });
  });
});
