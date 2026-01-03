// spec: frontend/specs/task-interactions.md
// seed: frontend/tests/seed.spec.ts

import { test, expect } from '@playwright/test';

test.describe('Start Implementation Flow', () => {
  test('View Ready to Implement State', async ({ page }) => {
    await page.goto('http://localhost:3031');
    await expect(page.getByRole('heading', { name: '$ mainloop' })).toBeVisible();
    
    // 1. Find task in "ready_to_implement" status
    const readyBadge = page.locator('text=READY').first();
    await expect(readyBadge).toBeVisible();

    // 2. Observe the task card
    const taskCard = readyBadge.locator('..').locator('..');
    
    // Expected: Status badge shows "READY" or similar
    await expect(readyBadge).toBeVisible();
    await expect(readyBadge).toHaveClass(/border-term-accent-alt/);
    await expect(readyBadge).toHaveClass(/text-term-accent-alt/);
    
    // Expand to see details
    await taskCard.click();
    
    // Expected: "Start Implementation" button prominently displayed
    const startButton = page.locator('button:has-text("Start Implementation")');
    await expect(startButton).toBeVisible();
    await expect(startButton).toHaveClass(/border-term-accent-alt/);
    
    // Expected: Approved plan summary may be visible
    const planContent = page.locator('.prose-terminal').first();
    await expect(planContent).toBeVisible();
    
    // Expected: Cancel option still available
    const cancelButton = page.locator('button:has-text("Cancel")');
    await expect(cancelButton).toBeVisible();
    
    // Verify success message about plan approval
    await expect(page.locator('text=Plan approved')).toBeVisible();
  });
});