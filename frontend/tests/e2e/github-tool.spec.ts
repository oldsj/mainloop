import { test, expect, chat } from '../fixtures';

/**
 * GitHub tool E2E test - verifies the chat agent can access GitHub.
 *
 * Uses real Claude API with mocked GitHub API (USE_MOCK_GITHUB=true).
 */

test.describe('GitHub Tool', () => {
  test.setTimeout(90000);

  test('uses github tool for info requests', async ({ appPage }) => {
    const response = await chat(appPage, 'List open issues on https://github.com/oldsj/mainloop');

    // Should have access and return issue info
    expect(response.toLowerCase()).not.toContain("don't have access");
    expect(response.toLowerCase()).not.toContain('cannot access');
    expect(response.toLowerCase()).toContain('issue');

    // Should NOT offer to spawn a task for info requests
    expect(response.toLowerCase()).not.toContain('spawn a task');
    expect(response.toLowerCase()).not.toContain('spawn a worker');
  });
});
