import { test, expect } from '@playwright/test';

/**
 * App health tests - verify UI elements render correctly.
 *
 * Note: API health + database reset are handled by global.setup.ts
 * These tests focus on UI rendering after API is confirmed working.
 */

test.describe('App Health', () => {
  test('app loads and shows main UI', async ({ page }) => {
    await page.goto('/');

    // App shell loads
    await expect(page.getByRole('heading', { name: '$ mainloop' }).first()).toBeVisible({
      timeout: 15000
    });

    // Chat area present
    await expect(page.getByText('$ mainloop --help').first()).toBeVisible();

    // Inbox panel visible (desktop)
    await expect(page.getByText('[INBOX]').first()).toBeVisible();
  });

  test('input field is focusable', async ({ page }) => {
    await page.goto('/');
    await expect(page.getByRole('heading', { name: '$ mainloop' }).first()).toBeVisible();

    // Input field should be visible and focusable
    const input = page.getByPlaceholder('Enter command...').first();
    await expect(input).toBeVisible();
    await input.focus();
    await expect(input).toBeFocused();
  });

  test('SSE connection establishes (no EventSource errors)', async ({ page }) => {
    const sseErrors: string[] = [];

    // Listen for console errors before navigating
    page.on('console', (msg) => {
      if (msg.type() === 'error' && msg.text().includes('EventSource')) {
        sseErrors.push(msg.text());
      }
    });

    await page.goto('/');
    await expect(page.getByRole('heading', { name: '$ mainloop' }).first()).toBeVisible();

    // Wait for inbox to load (confirms SSE/API working)
    await expect(page.getByText('[INBOX]').first()).toBeVisible();

    // No SSE errors should have occurred
    expect(sseErrors).toHaveLength(0);
  });
});
