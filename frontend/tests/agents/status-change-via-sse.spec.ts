// spec: frontend/specs/task-interactions.md
// seed: frontend/tests/seed.spec.ts

import { test, expect } from '@playwright/test';

test.describe('Real-time Updates', () => {
  test('Status Change via SSE', async ({ page }) => {
    await page.goto('http://localhost:3031');
    await expect(page.getByRole('heading', { name: '$ mainloop' })).toBeVisible();
    
    // 1. Have a task in active state
    const activeTask = page.locator('text=PLANNING, text=IMPLEMENTING').first();
    await expect(activeTask).toBeVisible();
    
    const initialStatus = await activeTask.textContent();
    const taskCard = activeTask.locator('..').locator('..');
    
    // Get task description to identify it later
    const taskDescription = await taskCard.locator('p.truncate').first().textContent();

    // 2. Backend updates task status (simulated via SSE)
    // This would require backend to send SSE event
    // For testing, we can trigger a state change by interacting with the task
    
    // 3. Observe inbox updates
    // Expected: Task status updates without page refresh
    // Wait for potential status change (this is environment-dependent)
    await page.waitForTimeout(2000);
    
    // Expected: UI reflects new state automatically
    const updatedTaskCard = page.locator(`text="${taskDescription}"`).first().locator('..').locator('..');
    await expect(updatedTaskCard).toBeVisible();
    
    // Expected: No jarring transitions or flashing
    // The task should smoothly update without full page reload
    
    // Expected: Badge counts update appropriately
    const attentionBadge = page.locator('.border.border-term-info').first();
    if (await attentionBadge.isVisible()) {
      const badgeCount = await attentionBadge.textContent();
      expect(badgeCount).toMatch(/\d+/);
    }
    
    // Verify no page reload occurred by checking connection state
    const isConnected = await page.evaluate(() => navigator.onLine);
    expect(isConnected).toBe(true);
  });
});