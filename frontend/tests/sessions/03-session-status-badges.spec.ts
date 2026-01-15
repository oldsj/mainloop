import { test, expect, seedSession } from '../fixtures';

/**
 * Test: Session status badges display correctly
 */
test.describe('Session status badges', () => {
  test('shows PENDING badge for pending session', async ({ appPage, userId }) => {
    await seedSession(appPage, userId, {
      status: 'pending',
      title: 'Pending Session'
    });

    await appPage.reload();
    await expect(appPage.getByRole('heading', { name: '$ mainloop' }).first()).toBeVisible();

    await expect(appPage.getByText('PENDING', { exact: true })).toBeVisible({ timeout: 10000 });
  });

  test('shows ACTIVE badge for active session', async ({ appPage, userId }) => {
    await seedSession(appPage, userId, {
      status: 'active',
      title: 'Active Session'
    });

    await appPage.reload();
    await expect(appPage.getByRole('heading', { name: '$ mainloop' }).first()).toBeVisible();

    await expect(appPage.getByText('ACTIVE', { exact: true })).toBeVisible({ timeout: 10000 });
  });

  test('shows NEEDS INPUT badge for waiting_on_user session', async ({ appPage, userId }) => {
    await seedSession(appPage, userId, {
      status: 'waiting_on_user',
      title: 'Waiting Session'
    });

    await appPage.reload();
    await expect(appPage.getByRole('heading', { name: '$ mainloop' }).first()).toBeVisible();

    await expect(appPage.getByText('NEEDS INPUT')).toBeVisible({ timeout: 10000 });
  });

  test('shows DONE badge for completed session', async ({ appPage, userId }) => {
    await seedSession(appPage, userId, {
      status: 'completed',
      title: 'Completed Session',
      summary: 'All tasks done successfully'
    });

    await appPage.reload();
    await expect(appPage.getByRole('heading', { name: '$ mainloop' }).first()).toBeVisible();

    await expect(appPage.getByText('DONE')).toBeVisible({ timeout: 10000 });
  });

  test('shows FAILED badge for failed session', async ({ appPage, userId }) => {
    await seedSession(appPage, userId, {
      status: 'failed',
      title: 'Failed Session',
      error: 'Something went wrong'
    });

    await appPage.reload();
    await expect(appPage.getByRole('heading', { name: '$ mainloop' }).first()).toBeVisible();

    await expect(appPage.getByText('FAILED', { exact: true })).toBeVisible({ timeout: 10000 });
  });

  test('shows error message for failed session', async ({ appPage, userId }) => {
    await seedSession(appPage, userId, {
      status: 'failed',
      title: 'Error Session',
      error: 'Connection timeout'
    });

    await appPage.reload();
    await expect(appPage.getByRole('heading', { name: '$ mainloop' }).first()).toBeVisible();

    // Should show error text
    await expect(appPage.getByText('Error: Connection timeout')).toBeVisible({ timeout: 10000 });
  });
});
