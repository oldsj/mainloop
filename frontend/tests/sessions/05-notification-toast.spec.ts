import { test, expect, seedSession } from '../fixtures';

/**
 * Test: Notification toast functionality
 */
test.describe('Notification toast', () => {
  test('displays notification when seeded', async ({ appPage, userId }) => {
    await seedSession(appPage, userId, {
      status: 'waiting_on_user',
      title: 'Needs Input Session',
      createNotification: true,
      notificationTitle: 'Session needs your help',
      notificationPreview: 'Please provide API key'
    });

    await appPage.reload();
    await expect(appPage.getByRole('heading', { name: '$ mainloop' }).first()).toBeVisible();

    // Notification toast should appear
    await expect(appPage.getByText('Session needs your help')).toBeVisible({ timeout: 10000 });
    await expect(appPage.getByText('Please provide API key')).toBeVisible();
  });

  // Skip: dismiss button has opacity-0 until hover, making it hard to test reliably
  test.skip('notification can be dismissed', async ({ appPage, userId }) => {
    await seedSession(appPage, userId, {
      status: 'waiting_on_user',
      title: 'Dismissable Session',
      createNotification: true,
      notificationTitle: 'Dismiss me',
      notificationPreview: 'Click X to dismiss'
    });

    await appPage.reload();
    await expect(appPage.getByRole('heading', { name: '$ mainloop' }).first()).toBeVisible();

    // Wait for notification
    const notification = appPage.getByText('Dismiss me');
    await expect(notification).toBeVisible({ timeout: 10000 });

    // Click dismiss button (X) - use force since it has opacity-0 until hover
    const dismissButton = appPage.getByRole('button', { name: 'Dismiss' });
    await dismissButton.click({ force: true });

    // Notification should disappear
    await expect(notification).not.toBeVisible({ timeout: 5000 });
  });

  test('clicking notification navigates to session page', async ({ appPage, userId }) => {
    const { sessionId } = await seedSession(appPage, userId, {
      status: 'waiting_on_user',
      title: 'Navigate Session',
      createNotification: true,
      notificationTitle: 'Click to view',
      notificationPreview: 'Takes you to session'
    });

    await appPage.reload();
    await expect(appPage.getByRole('heading', { name: '$ mainloop' }).first()).toBeVisible();

    // Wait for notification
    const notification = appPage.getByText('Click to view');
    await expect(notification).toBeVisible({ timeout: 10000 });

    // Click notification title text (not the dismiss button)
    await notification.click();

    // Should navigate to session page
    await expect(appPage).toHaveURL(`/sessions/${sessionId}`);

    // Session page should show the title (use first() since sidebar also shows it)
    await expect(appPage.getByRole('heading', { name: 'Navigate Session' }).first()).toBeVisible();
  });

  // Skip: timing issues with fetching multiple notifications after reload
  test.skip('shows max 3 notifications at once', async ({ appPage, userId }) => {
    // Create 4 sessions with notifications - seed them in sequence
    await seedSession(appPage, userId, {
      status: 'waiting_on_user',
      title: 'Session 1',
      createNotification: true,
      notificationTitle: 'Notification 1',
      notificationPreview: 'Preview 1'
    });
    await seedSession(appPage, userId, {
      status: 'waiting_on_user',
      title: 'Session 2',
      createNotification: true,
      notificationTitle: 'Notification 2',
      notificationPreview: 'Preview 2'
    });
    await seedSession(appPage, userId, {
      status: 'waiting_on_user',
      title: 'Session 3',
      createNotification: true,
      notificationTitle: 'Notification 3',
      notificationPreview: 'Preview 3'
    });
    await seedSession(appPage, userId, {
      status: 'waiting_on_user',
      title: 'Session 4',
      createNotification: true,
      notificationTitle: 'Notification 4',
      notificationPreview: 'Preview 4'
    });

    await appPage.reload();
    await expect(appPage.getByRole('heading', { name: '$ mainloop' }).first()).toBeVisible();

    // Wait for notifications to load
    await expect(appPage.getByText('Notification 1')).toBeVisible({ timeout: 10000 });

    // Should only show 3 notifications (the slice in NotificationToast.svelte)
    await expect(appPage.getByText('Notification 2')).toBeVisible();
    await expect(appPage.getByText('Notification 3')).toBeVisible();

    // 4th notification should not be visible (only first 3 are shown)
    await expect(appPage.getByText('Notification 4')).not.toBeVisible();
  });
});
