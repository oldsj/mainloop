import { test, expect } from '../fixtures';

test.describe('Mobile Tab Navigation', () => {
  test('can switch between chat and inbox', async ({ appPage: page }) => {
    const chatInput = page.getByTestId('command-input');
    const inboxHeading = page.getByRole('heading', { name: '[INBOX]' });

    await expect(chatInput).toBeVisible();

    // Go to inbox
    await page.getByTestId('tab-inbox').click();
    await expect(inboxHeading).toBeVisible();

    // Go back to chat
    await page.getByTestId('tab-chat').click();
    await expect(chatInput).toBeVisible();
  });

  test('typed text persists across tab switches', async ({ appPage: page }) => {
    const chatInput = page.getByTestId('command-input');

    // Type something
    await chatInput.fill('my draft message');

    // Switch away and back
    await page.getByTestId('tab-inbox').click();
    await expect(page.getByRole('heading', { name: '[INBOX]' })).toBeVisible();

    await page.getByTestId('tab-chat').click();
    await expect(chatInput).toHaveValue('my draft message');
  });
});
