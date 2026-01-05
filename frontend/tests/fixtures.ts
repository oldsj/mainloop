import { test as base, expect, type Page } from '@playwright/test';

/**
 * Shared fixtures for mainloop E2E tests.
 *
 * Key principles:
 * - No waitForTimeout() - always wait for specific conditions
 * - Single API_URL env var - no port mapping magic
 * - Fixtures are composable and reusable
 */

// API URL from environment (set by make test or CI)
export const apiURL = process.env.API_URL || 'http://localhost:8000';

/**
 * Extended test with common fixtures
 */
export const test = base.extend<{
  /** Page with app loaded and ready for interaction */
  appPage: Page;
}>({
  appPage: async ({ page }, use) => {
    await page.goto('/');

    // Wait for app shell to render
    await expect(page.getByRole('heading', { name: '$ mainloop' }).first()).toBeVisible({
      timeout: 15000
    });

    // Wait for SSE connection by checking for a network request or UI indicator
    // Instead of waitForTimeout, we wait for the inbox to show (requires API connection)
    await expect(page.getByText('[INBOX]').first()).toBeVisible();

    await use(page);
  }
});

export { expect };

/**
 * Reset database to clean state.
 * Call this in beforeEach when test needs isolated state.
 */
export async function resetTestData(): Promise<void> {
  const response = await fetch(`${apiURL}/internal/test/reset`, { method: 'POST' });
  if (!response.ok) {
    throw new Error(`Failed to reset test data: ${response.status}`);
  }
}

/**
 * Send a chat message and wait for it to appear.
 * Does NOT wait for response - use waitForResponse() for that.
 */
export async function sendMessage(page: Page, message: string): Promise<void> {
  const input = page.getByPlaceholder('Enter command...').first();
  await input.click();
  await input.fill(message);
  await page.getByRole('button', { name: 'EXEC' }).click();

  // Wait for our message to appear in the chat
  await expect(page.getByText(message).first()).toBeVisible({ timeout: 5000 });
}

/**
 * Wait for an assistant response to appear after sending a message.
 * Returns the response text.
 */
export async function waitForResponse(page: Page): Promise<string> {
  // Wait for assistant message to appear (identified by bg-term-bg-secondary class)
  const assistantMessage = page.locator('.message.bg-term-bg-secondary').last();
  await expect(assistantMessage).toBeVisible({ timeout: 30000 });

  return (await assistantMessage.textContent()) || '';
}

/**
 * Send message and wait for response. Combines sendMessage + waitForResponse.
 */
export async function chat(page: Page, message: string): Promise<string> {
  await sendMessage(page, message);
  return await waitForResponse(page);
}

/**
 * Get count of messages in chat.
 */
export async function getMessageCount(page: Page): Promise<number> {
  // Messages have class "message" - both user and assistant
  const messages = page.locator('.message');
  return await messages.count();
}

/**
 * Seed a task in specific state. Returns task ID.
 */
export async function seedTask(
  page: Page,
  options: {
    status: 'waiting_plan_review' | 'waiting_questions' | 'implementing' | 'ready_to_implement';
    description?: string;
    plan?: string;
    questions?: Array<{
      id: string;
      question: string;
      options: Array<{ id: string; label: string }>;
    }>;
  }
): Promise<string> {
  const response = await page.request.post(`${apiURL}/internal/test/seed-task`, {
    data: {
      status: options.status,
      task_type: 'feature',
      description: options.description || 'Test task',
      repo_url: 'https://github.com/test/repo',
      plan: options.plan,
      questions: options.questions
    }
  });

  if (!response.ok()) {
    throw new Error(`Failed to seed task: ${response.status()}`);
  }

  const data = await response.json();
  return data.task_id;
}

/**
 * Wait for inbox to show a task with specific status badge.
 */
export async function waitForTaskInInbox(
  page: Page,
  statusText: string,
  timeout = 10000
): Promise<void> {
  await expect(page.getByText(statusText).first()).toBeVisible({ timeout });
}

/**
 * Navigate to inbox (mobile) or ensure inbox is visible (desktop).
 */
export async function openInbox(page: Page): Promise<void> {
  // Check if we're on mobile (inbox tab visible)
  const inboxTab = page.getByRole('button', { name: 'Inbox' });
  if (await inboxTab.isVisible()) {
    await inboxTab.click();
    await expect(page.getByText('[INBOX]').first()).toBeVisible();
  }
  // On desktop, inbox is always visible
}

/**
 * Set up a conversation with a greeting message.
 * Resets DB, sends "hello", waits for response.
 */
export async function setupConversation(page: Page): Promise<void> {
  // Reset database for clean state
  await resetTestData();

  // Reload to pick up clean state
  await page.reload();
  await expect(page.getByRole('heading', { name: '$ mainloop' }).first()).toBeVisible();

  // Send hello message
  const input = page.getByPlaceholder('Enter command...').first();
  await input.click();
  await input.fill('hello');
  await page.getByRole('button', { name: 'EXEC' }).click();

  // Wait for message to appear in chat
  await expect(page.getByText('hello').first()).toBeVisible({ timeout: 10000 });

  // Wait for assistant response (mock claude should respond quickly)
  // Assistant messages have bg-term-bg-secondary class
  const assistantMessage = page.locator('.message.bg-term-bg-secondary').first();
  await expect(assistantMessage).toBeVisible({ timeout: 30000 });
}
