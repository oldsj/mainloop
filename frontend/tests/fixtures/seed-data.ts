/**
 * Test data seeding helpers.
 *
 * Uses API_URL env var directly - no port mapping magic.
 */

import type { Page } from '@playwright/test';

// Single source of truth for API URL
const apiURL = process.env.API_URL || 'http://localhost:8000';

/**
 * Reset test database for clean state
 */
export async function resetTestData(page: Page): Promise<void> {
  await page.request.post(`${apiURL}/internal/test/reset`);
}

/**
 * Seed a task in "waiting_plan_review" status (REVIEW PLAN badge)
 */
export async function seedTaskWaitingPlanReview(page: Page) {
  await resetTestData(page);

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
 * Seed a task in "waiting_questions" status (NEEDS INPUT badge)
 */
export async function seedTaskWaitingQuestions(page: Page) {
  await resetTestData(page);

  const response = await page.request.post(`${apiURL}/internal/test/seed-task`, {
    data: {
      status: 'waiting_questions',
      task_type: 'feature',
      description: 'Add user authentication',
      repo_url: 'https://github.com/test/repo',
      questions: [
        {
          id: 'q1',
          header: 'Authentication Method',
          question: 'Which authentication method should we use?',
          options: [
            { id: 'jwt', label: 'JWT tokens', description: 'Stateless, good for APIs' },
            { id: 'session', label: 'Session cookies', description: 'Server-side sessions' },
            { id: 'oauth', label: 'OAuth 2.0', description: 'Third-party providers' }
          ]
        },
        {
          id: 'q2',
          header: 'Rate Limiting',
          question: 'Should we add rate limiting?',
          options: [
            { id: 'yes', label: 'Yes', description: 'Prevent abuse' },
            { id: 'no', label: 'No', description: 'Keep it simple' },
            { id: 'later', label: 'Add later', description: 'Start without, add if needed' }
          ]
        }
      ]
    }
  });

  if (!response.ok()) {
    throw new Error(`Failed to seed task: ${response.status()}`);
  }

  return response.json();
}

/**
 * Seed a task in "implementing" status (WORKING badge)
 */
export async function seedTaskImplementing(page: Page) {
  await resetTestData(page);

  const response = await page.request.post(`${apiURL}/internal/test/seed-task`, {
    data: {
      status: 'implementing',
      task_type: 'feature',
      description: 'Add user authentication',
      repo_url: 'https://github.com/test/repo'
    }
  });

  if (!response.ok()) {
    throw new Error(`Failed to seed task: ${response.status()}`);
  }

  return response.json();
}

/**
 * Seed a task in "ready_to_implement" status (READY badge)
 */
export async function seedTaskReadyToImplement(page: Page) {
  await resetTestData(page);

  const response = await page.request.post(`${apiURL}/internal/test/seed-task`, {
    data: {
      status: 'ready_to_implement',
      task_type: 'feature',
      description: 'Add user authentication',
      repo_url: 'https://github.com/test/repo',
      plan: `# Implementation Plan\n\n## Steps\n1. Create User model\n2. Add auth endpoints`
    }
  });

  if (!response.ok()) {
    throw new Error(`Failed to seed task: ${response.status()}`);
  }

  return response.json();
}
