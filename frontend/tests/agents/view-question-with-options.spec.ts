// spec: frontend/specs/task-interactions.md
// seed: frontend/tests/seed.spec.ts

import { test, expect } from '@playwright/test';

test.describe('Question Answering Flow', () => {
  test('View Question with Options', async ({ page }) => {
    // 1. Expand a task in "waiting_questions" status (NEEDS INPUT badge)
    await page.goto('http://localhost:3031');
    await expect(page.getByRole('heading', { name: '$ mainloop' })).toBeVisible();
    
    // Find task with NEEDS INPUT status
    const needsInputBadge = page.locator('text=NEEDS INPUT').first();
    await expect(needsInputBadge).toBeVisible();
    
    // Click on the task to expand it
    const taskCard = needsInputBadge.locator('..').locator('..');
    await taskCard.click();

    // 2. Observe the question display
    // Expected: Question text displayed clearly
    await expect(page.locator('.prose-terminal').first()).toBeVisible();
    
    // Expected: Multiple choice options shown as clickable buttons
    const optionButtons = page.locator('button:has-text("Yes"), button:has-text("No")');
    await expect(optionButtons.first()).toBeVisible();
    
    // Expected: Custom text input available as alternative
    const customInput = page.getByPlaceholder('Or type a custom answer...');
    await expect(customInput).toBeVisible();
    
    // Expected: Question header indicates question number (e.g., "1 of 3")
    const questionNumber = page.locator('span').filter({ hasText: /^1$/ }).first();
    await expect(questionNumber).toBeVisible();
  });
});