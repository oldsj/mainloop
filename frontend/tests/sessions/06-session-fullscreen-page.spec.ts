import { test, expect, seedSession } from '../fixtures';

/**
 * Test: Session fullscreen page
 *
 * Note: The sidebar also shows sessions, so we need to scope to main content area.
 */
test.describe('Session fullscreen page', () => {
  test('displays session title and description', async ({ appPage, userId }) => {
    const { sessionId } = await seedSession(appPage, userId, {
      status: 'active',
      title: 'Full Page Session',
      description: 'Detailed view test'
    });

    // Navigate directly to session page
    await appPage.goto(`/sessions/${sessionId}`);

    // Should show title in main content (use first() to avoid sidebar match)
    await expect(appPage.getByRole('heading', { name: 'Full Page Session' }).first()).toBeVisible({
      timeout: 10000
    });
    // Description appears in multiple places, just check it exists
    await expect(appPage.getByText('Detailed view test').first()).toBeVisible();
  });

  test('shows back button that returns to home', async ({ appPage, userId }) => {
    const { sessionId } = await seedSession(appPage, userId, {
      status: 'active',
      title: 'Back Button Session'
    });

    await appPage.goto(`/sessions/${sessionId}`);
    await expect(appPage.getByRole('heading', { name: 'Back Button Session' }).first()).toBeVisible(
      {
        timeout: 10000
      }
    );

    // Click back button
    await appPage.getByRole('link', { name: 'Back' }).click();

    // Should be back at home
    await expect(appPage).toHaveURL('/');
    await expect(appPage.getByRole('heading', { name: '$ mainloop' }).first()).toBeVisible();
  });

  test('shows status badge', async ({ appPage, userId }) => {
    const { sessionId } = await seedSession(appPage, userId, {
      status: 'active',
      title: 'Status Badge Session'
    });

    await appPage.goto(`/sessions/${sessionId}`);
    await expect(
      appPage.getByRole('heading', { name: 'Status Badge Session' }).first()
    ).toBeVisible({
      timeout: 10000
    });

    // Should show status badge (may appear multiple times - in sidebar and main)
    await expect(appPage.getByText('[ACTIVE]').first()).toBeVisible();
  });

  test('shows cancel button for active session', async ({ appPage, userId }) => {
    const { sessionId } = await seedSession(appPage, userId, {
      status: 'active',
      title: 'Cancel Button Session'
    });

    await appPage.goto(`/sessions/${sessionId}`);
    await expect(
      appPage.getByRole('heading', { name: 'Cancel Button Session' }).first()
    ).toBeVisible({
      timeout: 10000
    });

    // Cancel button may be in both sidebar expanded view and main content
    await expect(appPage.getByRole('button', { name: 'Cancel' }).first()).toBeVisible();
  });

  test('shows summary for completed session', async ({ appPage, userId }) => {
    const { sessionId } = await seedSession(appPage, userId, {
      status: 'completed',
      title: 'Completed With Summary',
      summary: 'Successfully updated the documentation with new API examples.'
    });

    await appPage.goto(`/sessions/${sessionId}`);
    await expect(
      appPage.getByRole('heading', { name: 'Completed With Summary' }).first()
    ).toBeVisible({
      timeout: 10000
    });

    // Should show summary section (h3 element)
    await expect(appPage.getByRole('heading', { name: 'Summary', exact: true })).toBeVisible();
    await expect(
      appPage.getByText('Successfully updated the documentation with new API examples.')
    ).toBeVisible();
  });

  test('shows error for failed session', async ({ appPage, userId }) => {
    const { sessionId } = await seedSession(appPage, userId, {
      status: 'failed',
      title: 'Failed Session',
      error: 'API rate limit exceeded after 100 requests'
    });

    await appPage.goto(`/sessions/${sessionId}`);
    await expect(appPage.getByRole('heading', { name: 'Failed Session' }).first()).toBeVisible({
      timeout: 10000
    });

    // Should show error section (h3 element)
    await expect(appPage.getByRole('heading', { name: 'Error', exact: true })).toBeVisible();
    // Error text appears in both main and sidebar, use first()
    await expect(
      appPage.getByText('API rate limit exceeded after 100 requests').first()
    ).toBeVisible();
  });

  test('shows 404 for non-existent session', async ({ appPage }) => {
    await appPage.goto('/sessions/non-existent-session-id');

    // Should show error message
    await expect(appPage.getByText('Session not found')).toBeVisible({ timeout: 10000 });
    await expect(appPage.getByRole('link', { name: 'Back to home' })).toBeVisible();
  });

  test('has chat and logs tabs', async ({ appPage, userId }) => {
    const { sessionId } = await seedSession(appPage, userId, {
      status: 'active',
      title: 'Tabs Session'
    });

    await appPage.goto(`/sessions/${sessionId}`);
    await expect(appPage.getByRole('heading', { name: 'Tabs Session' }).first()).toBeVisible({
      timeout: 10000
    });

    // Tabs may appear in both sidebar and main - just verify they exist
    await expect(appPage.getByRole('button', { name: 'Chat' }).first()).toBeVisible();
    await expect(appPage.getByRole('button', { name: 'Logs' }).first()).toBeVisible();
  });
});
