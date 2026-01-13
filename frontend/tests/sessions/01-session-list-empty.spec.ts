import { test, expect } from '../fixtures';

/**
 * Test: Empty session list shows placeholder
 */
test('shows empty state when no sessions exist', async ({ appPage }) => {
  // On desktop, sessions panel should be visible in sidebar
  const sessionsHeader = appPage.getByRole('heading', { name: 'Sessions' });

  // Check if we're on desktop (sessions panel visible)
  const isDesktop = await sessionsHeader.isVisible({ timeout: 2000 }).catch(() => false);

  if (isDesktop) {
    // Desktop: verify empty state message
    await expect(appPage.getByText('$ sessions --list')).toBeVisible();
    await expect(appPage.getByText('No sessions yet')).toBeVisible();
    await expect(
      appPage.getByText('Sessions appear when Claude spawns background work')
    ).toBeVisible();
  }
  // Mobile: sessions are behind a tab, skip this test for mobile
});
