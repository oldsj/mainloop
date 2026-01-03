/**
 * Test data fixtures
 *
 * Helpers to seed database with tasks and queue items for testing.
 */

import type { Page } from '@playwright/test';

/**
 * Seed a task in "waiting_plan_review" status
 *
 * Creates a task with a plan ready for review, simulating what happens
 * when Claude finishes planning and needs human approval.
 */
export async function seedTaskWaitingPlanReview(page: Page) {
  // Get the base URL from the page
  const baseURL = new URL(page.url()).origin;
  const apiURL = baseURL.replace('3031', '8031'); // Test API on port 8031

  // Directly insert task into database via internal API
  const response = await page.request.post(`${apiURL}/internal/test/seed-task`, {
    data: {
      status: 'waiting_plan_review',
      task_type: 'feature',
      description: 'Add user authentication',
      repo_url: 'https://github.com/test/repo',
      plan: `# Implementation Plan

## Overview
Add JWT-based authentication to the application.

## Steps
1. Install required packages (PyJWT, passlib)
2. Create User model with password hashing
3. Implement login/register endpoints
4. Add middleware for auth verification
5. Add tests for auth flows

## Files to modify
- backend/models.py (new User model)
- backend/api.py (new auth endpoints)
- backend/middleware.py (new auth middleware)`
    }
  });

  if (!response.ok()) {
    throw new Error(`Failed to seed task: ${response.status()}`);
  }

  return response.json();
}

/**
 * Seed a queue item with questions
 */
export async function seedQueueItemQuestions(page: Page) {
  const baseURL = new URL(page.url()).origin;
  const apiURL = baseURL.replace('3031', '8031');

  const response = await page.request.post(`${apiURL}/internal/test/seed-queue-item`, {
    data: {
      type: 'question',
      content: {
        questions: [
          {
            id: 'q1',
            question: 'Which authentication method should we use?',
            options: [
              { id: 'jwt', label: 'JWT tokens' },
              { id: 'session', label: 'Server sessions' }
            ]
          }
        ]
      }
    }
  });

  if (!response.ok()) {
    throw new Error(`Failed to seed queue item: ${response.status()}`);
  }

  return response.json();
}
