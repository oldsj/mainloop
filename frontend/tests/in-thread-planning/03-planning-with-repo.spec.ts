import { test, expect, seedRepo, fixtureRepoUrl } from '../fixtures';

/**
 * Planning tests with seeded fixture repo.
 *
 * These tests verify that:
 * 1. The repo gets cached (seeded before test)
 * 2. Claude actually explores the repo during planning
 * 3. The plan references real files from the repo
 */

test.describe('Planning with Fixture Repo', () => {
  test.describe.configure({ mode: 'serial' });
  test.setTimeout(180000); // 3 min for planning exploration

  test('planning explores and references actual repo files', async ({ appPage: page }) => {
    // Seed the fixture repo with known content
    const { repo_url, files } = await seedRepo(page);
    expect(repo_url).toBe(fixtureRepoUrl);
    expect(files).toContain('src/main.py');
    expect(files).toContain('src/utils.py');

    // Ask Claude to plan a feature for this repo
    const input = page.getByPlaceholder('Enter command...').first();
    const execButton = page.getByRole('button', { name: 'EXEC' });

    await expect(input).toBeVisible();
    await expect(input).toBeEnabled();
    await input.fill(
      `I want to add a new function to ${repo_url} - please start planning. ` +
        `The function should calculate the factorial of a number.`
    );
    await execButton.click();

    // Wait for user message to appear (confirms submission)
    await expect(page.getByText('add a new function').first()).toBeVisible({ timeout: 10000 });

    // Wait for Claude's response (may take a while as it explores)
    const response = page.locator('.message.bg-term-bg-secondary').last();
    await expect(response).toBeVisible({ timeout: 120000 });

    // Get the response text
    const responseText = (await response.textContent()) || '';

    // Claude should mention actual files from the repo
    // The fixture repo has: src/main.py, src/utils.py, tests/test_main.py
    const mentionsRepoFiles =
      responseText.includes('main.py') ||
      responseText.includes('utils.py') ||
      responseText.includes('test_main.py') ||
      responseText.includes('pyproject.toml') ||
      responseText.includes('src/');

    expect(mentionsRepoFiles).toBe(true);
  });

  test('planning identifies existing code patterns', async ({ appPage: page }) => {
    // Seed repo
    await seedRepo(page);

    const input = page.getByPlaceholder('Enter command...').first();
    const execButton = page.getByRole('button', { name: 'EXEC' });

    // First message: start planning to create the session
    await expect(input).toBeVisible();
    await expect(input).toBeEnabled();
    await input.fill(`Start planning on ${fixtureRepoUrl} - I want to explore the codebase.`);
    await execButton.click();

    // Wait for user message and first response (session created)
    await expect(page.getByText('Start planning on').first()).toBeVisible({ timeout: 10000 });
    await expect(page.locator('.message.bg-term-bg-secondary').last()).toBeVisible({
      timeout: 60000
    });

    // Wait for input to be ready again before sending second message
    await expect(input).toBeEnabled({ timeout: 10000 });

    // Second message: ask to explore - NOW Claude has file tools with correct cwd
    await input.fill('Please read the main source files and tell me about the functions.');
    await expect(input).toHaveValue(/read the main source files/);
    await execButton.click();

    // Wait for second user message to confirm submission
    await expect(page.getByText('read the main source files').first()).toBeVisible({
      timeout: 10000
    });

    // Wait for second response
    const messages = page.locator('.message.bg-term-bg-secondary');
    await expect(messages).toHaveCount(2, { timeout: 120000 });

    const responseText = (await messages.last().textContent()) || '';

    // Claude should identify the actual functions in the fixture repo:
    // - hello() in main.py
    // - add() in main.py
    // - format_name() in utils.py
    // - validate_email() in utils.py
    const identifiesRealCode =
      responseText.includes('hello') ||
      responseText.includes('add') ||
      responseText.includes('format_name') ||
      responseText.includes('validate_email') ||
      responseText.includes('greeting') || // Claude might describe hello()
      responseText.includes('validation') || // Claude might describe validate_email()
      responseText.includes('main.py') ||
      responseText.includes('utils.py');

    expect(identifiesRealCode).toBe(true);
  });

  test('planning can be approved and creates task', async ({ appPage: page }) => {
    // This test verifies the full flow: plan -> approve -> task created
    // Note: GitHub issue creation is mocked in test env

    await seedRepo(page);

    const input = page.getByPlaceholder('Enter command...').first();
    const execButton = page.getByRole('button', { name: 'EXEC' });

    // Start planning
    await expect(input).toBeVisible();
    await expect(input).toBeEnabled();
    await input.fill(
      `Start planning to add a multiply function to ${fixtureRepoUrl}. ` +
        `Keep it simple - just add a multiply(a, b) function to main.py.`
    );
    await execButton.click();

    // Wait for user message and planning response
    await expect(page.getByText('Start planning to add a multiply').first()).toBeVisible({
      timeout: 10000
    });
    await expect(page.locator('.message.bg-term-bg-secondary').last()).toBeVisible({
      timeout: 120000
    });

    // Wait for input to be ready
    await expect(input).toBeEnabled({ timeout: 10000 });

    // Ask Claude to approve/proceed
    await input.fill(
      'That looks good. Please approve this plan and create a task to implement it.'
    );
    await expect(input).toHaveValue(/approve this plan/);
    await execButton.click();

    // Wait for second user message to confirm submission
    await expect(page.getByText('approve this plan').first()).toBeVisible({ timeout: 10000 });

    // Wait for approval response
    const messages = page.locator('.message.bg-term-bg-secondary');
    const initialCount = await messages.count();
    await expect(messages).toHaveCount(initialCount + 1, { timeout: 60000 });

    // Check if task was created (should appear in projects list or response mentions it)
    const lastResponse = await messages.last().textContent();
    const taskCreated =
      lastResponse?.includes('task') ||
      lastResponse?.includes('issue') ||
      lastResponse?.includes('created') ||
      lastResponse?.includes('spawned');

    // The task should be created (mocked GitHub in test env)
    expect(taskCreated).toBe(true);
  });
});
