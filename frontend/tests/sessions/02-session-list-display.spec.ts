import { test, expect, seedSession } from '../fixtures';

/**
 * Test: Session list displays seeded sessions
 */
test('displays session with title and description', async ({ appPage, userId }) => {
  // Seed a session
  await seedSession(appPage, userId, {
    status: 'active',
    title: 'Update Documentation',
    description: 'Adding API reference section'
  });

  // Refresh to load the session
  await appPage.reload();
  await expect(appPage.getByRole('heading', { name: '$ mainloop' }).first()).toBeVisible();

  // Session should appear in the list
  await expect(appPage.getByText('Update Documentation')).toBeVisible({ timeout: 10000 });
  await expect(appPage.getByText('Adding API reference section')).toBeVisible();
});

test('displays session active badge', async ({ appPage, userId }) => {
  // Seed an active session
  await seedSession(appPage, userId, {
    status: 'active',
    title: 'Active Session Test'
  });

  await appPage.reload();
  await expect(appPage.getByRole('heading', { name: '$ mainloop' }).first()).toBeVisible();

  // Check for ACTIVE status badge
  await expect(appPage.getByText('[ACTIVE]')).toBeVisible({ timeout: 10000 });
});

test('displays session count in header', async ({ appPage, userId }) => {
  // Seed multiple active sessions
  await seedSession(appPage, userId, {
    status: 'active',
    title: 'Session 1'
  });
  await seedSession(appPage, userId, {
    status: 'active',
    title: 'Session 2'
  });

  await appPage.reload();
  await expect(appPage.getByRole('heading', { name: '$ mainloop' }).first()).toBeVisible();

  // Header should show active count
  await expect(appPage.getByText('(2 active)')).toBeVisible({ timeout: 10000 });
});
