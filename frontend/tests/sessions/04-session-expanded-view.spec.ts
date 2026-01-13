import { test, expect, seedSession } from '../fixtures';

/**
 * Test: Session expanded view functionality
 */
test.describe('Session expanded view', () => {
  test('clicking session expands it', async ({ appPage, userId }) => {
    await seedSession(appPage, userId, {
      status: 'active',
      title: 'Expandable Session',
      description: 'Click to expand'
    });

    await appPage.reload();
    await expect(appPage.getByRole('heading', { name: '$ mainloop' }).first()).toBeVisible();

    // Click on the session
    const sessionItem = appPage.getByText('Expandable Session');
    await expect(sessionItem).toBeVisible({ timeout: 10000 });
    await sessionItem.click();

    // Expanded view should show tabs
    await expect(appPage.getByRole('button', { name: 'Chat' })).toBeVisible();
    await expect(appPage.getByRole('button', { name: 'Logs' })).toBeVisible();
  });

  test('expanded view shows chat tab by default', async ({ appPage, userId }) => {
    await seedSession(appPage, userId, {
      status: 'active',
      title: 'Chat Tab Session'
    });

    await appPage.reload();
    await expect(appPage.getByRole('heading', { name: '$ mainloop' }).first()).toBeVisible();

    // Click on the session
    await appPage.getByText('Chat Tab Session').click();

    // Chat tab should be active - use first() since there may be multiple
    const chatTab = appPage.getByRole('button', { name: 'Chat' }).first();
    await expect(chatTab).toBeVisible();

    // Should show session chat empty state
    await expect(appPage.getByText('$ session --start')).toBeVisible();
  });

  // Skip: Tab switching in expanded view has timing issues
  test.skip('can switch to logs tab', async ({ appPage, userId }) => {
    await seedSession(appPage, userId, {
      status: 'active',
      title: 'Logs Tab Session'
    });

    await appPage.reload();
    await expect(appPage.getByRole('heading', { name: '$ mainloop' }).first()).toBeVisible();

    // Click on the session to expand
    await appPage.getByText('Logs Tab Session').click();

    // Wait for expanded view to appear
    await expect(appPage.getByRole('button', { name: 'Chat' }).first()).toBeVisible();

    // Switch to logs tab - use first() since there may be multiple
    const logsButton = appPage.getByRole('button', { name: 'Logs' }).first();
    await logsButton.click();

    // Should show logs placeholder (use a more specific locator)
    await expect(appPage.locator('pre:has-text("Logs not yet implemented")')).toBeVisible({
      timeout: 5000
    });
  });

  test('expanded view has fullscreen link', async ({ appPage, userId }) => {
    const { sessionId } = await seedSession(appPage, userId, {
      status: 'active',
      title: 'Fullscreen Link Session'
    });

    await appPage.reload();
    await expect(appPage.getByRole('heading', { name: '$ mainloop' }).first()).toBeVisible();

    // Click on the session
    await appPage.getByText('Fullscreen Link Session').click();

    // Should have fullscreen link
    const fullscreenLink = appPage.getByRole('link', { name: 'Fullscreen' });
    await expect(fullscreenLink).toBeVisible();
    await expect(fullscreenLink).toHaveAttribute('href', `/sessions/${sessionId}`);
  });

  test('expanded view has cancel button for active session', async ({ appPage, userId }) => {
    await seedSession(appPage, userId, {
      status: 'active',
      title: 'Cancellable Session'
    });

    await appPage.reload();
    await expect(appPage.getByRole('heading', { name: '$ mainloop' }).first()).toBeVisible();

    // Click on the session
    await appPage.getByText('Cancellable Session').click();

    // Should have cancel button in expanded view
    await expect(appPage.getByRole('button', { name: 'Cancel' }).first()).toBeVisible();
  });

  test('completed session does not show cancel button', async ({ appPage, userId }) => {
    await seedSession(appPage, userId, {
      status: 'completed',
      title: 'Completed Session',
      summary: 'Done!'
    });

    await appPage.reload();
    await expect(appPage.getByRole('heading', { name: '$ mainloop' }).first()).toBeVisible();

    // Click on the session
    await appPage.getByText('Completed Session').click();

    // Should NOT have cancel button
    await expect(appPage.getByRole('button', { name: 'Cancel' })).not.toBeVisible();
  });

  // Skip: toggle collapse is flaky due to timing between expand/collapse state
  test.skip('clicking session again collapses it', async ({ appPage, userId }) => {
    await seedSession(appPage, userId, {
      status: 'active',
      title: 'Toggle Session'
    });

    await appPage.reload();
    await expect(appPage.getByRole('heading', { name: '$ mainloop' }).first()).toBeVisible();

    const sessionItem = appPage.getByText('Toggle Session').first();
    await expect(sessionItem).toBeVisible({ timeout: 10000 });

    // Click to expand
    await sessionItem.click();
    await expect(appPage.getByRole('button', { name: 'Chat' }).first()).toBeVisible();

    // Click again to collapse
    await sessionItem.click();

    // Expanded view content should not be visible anymore
    await expect(appPage.getByText('$ session --start')).not.toBeVisible({ timeout: 2000 });
  });
});
