import { test, expect } from '../fixtures';

test.describe('Inbox Panel Visibility', () => {
  test('Desktop Inbox Always Visible', async ({ appPage: page }) => {
    // Verify inbox panel header shows "[INBOX]" with terminal styling
    await expect(page.getByText('[INBOX]')).toBeVisible();

    // Verify inbox panel is visible on right side showing empty state
    await expect(page.getByText('All caught up')).toBeVisible();
  });
});
