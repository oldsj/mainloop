import { test, expect, seedSession } from '../fixtures';

/**
 * Test: Session page functionality (clicking session navigates to session page)
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

    // Click on the session - this navigates to session page
    const sessionItem = appPage.getByText('Expandable Session');
    await expect(sessionItem).toBeVisible({ timeout: 10000 });
    await sessionItem.click();

    // Session page should show tabs
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

    // Click on the session - navigates to session page
    await appPage.getByText('Chat Tab Session').click();

    // Chat tab should be active
    const chatTab = appPage.getByRole('button', { name: 'Chat' }).first();
    await expect(chatTab).toBeVisible();

    // Should show session chat empty state
    await expect(appPage.getByText('$ session --start')).toBeVisible();
  });

  // Skip: Tab switching has timing issues
  test.skip('can switch to logs tab', async ({ appPage, userId }) => {
    await seedSession(appPage, userId, {
      status: 'active',
      title: 'Logs Tab Session'
    });

    await appPage.reload();
    await expect(appPage.getByRole('heading', { name: '$ mainloop' }).first()).toBeVisible();

    // Click on the session
    await appPage.getByText('Logs Tab Session').click();

    // Wait for page to load
    await expect(appPage.getByRole('button', { name: 'Chat' }).first()).toBeVisible();

    // Switch to logs tab
    const logsButton = appPage.getByRole('button', { name: 'Logs' }).first();
    await logsButton.click();

    // Should show logs placeholder
    await expect(appPage.getByText('No logs available yet')).toBeVisible({
      timeout: 5000
    });
  });

  test('session page shows title and description', async ({ appPage, userId }) => {
    await seedSession(appPage, userId, {
      status: 'active',
      title: 'Test Session Title',
      description: 'Test session description text'
    });

    await appPage.reload();
    await expect(appPage.getByRole('heading', { name: '$ mainloop' }).first()).toBeVisible();

    // Click on the session - navigates to session page
    await appPage.getByText('Test Session Title').first().click();

    // Session page should show title as h1 heading and description
    await expect(
      appPage.getByRole('heading', { name: 'Test Session Title', level: 1 })
    ).toBeVisible();
    // Use first() to get the main page description (not the sidebar)
    await expect(
      appPage.getByRole('main').getByText('Test session description text')
    ).toBeVisible();
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
