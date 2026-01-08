# CLAUDE.md

## Project Overview

**mainloop** is an attention management system - one place to focus, accessible from any device.

- One continuous conversation that persists across devices
- AI workers handle tasks in background
- Unified inbox surfaces what needs attention

**Structure:**

```text
mainloop/
├── backend/      # Python 3.13+ FastAPI + PostgreSQL
├── frontend/     # TypeScript/SvelteKit 5 + Tailwind v4
├── claude-agent/ # Claude Code CLI container
├── models/       # Shared Pydantic models
└── k8s/          # Kubernetes manifests
```

## Development

```bash
make dev          # Start all services (frontend :3000, backend :8000)
make fmt          # Format before committing
make lint-all     # Check for issues
```

**Backend:** `uv add <package>` for dependencies (never edit pyproject.toml manually)

**Frontend:** `pnpm add <package>` for dependencies

## Testing

```bash
make test         # Start services + Playwright UI (keep running)
make test-run     # Run tests headless (separate terminal)
make test-reset   # Clear DB + namespaces between runs
```

**Workflow:**

1. Run `make test` once - starts Kind cluster (backend :8081, frontend :5173)
2. Wait for pods to be ready before running tests
3. Use `make test-run` for quick iterations

**Before running tests**, verify deployments are ready:

```bash
kubectl get pods -n mainloop --context kind-mainloop-test -w
```

Wait for new pods to show `Running` and old pods to terminate.

**Common issues:**

- Port already in use → kill orphan processes or restart `make test`
- Tests fail on old code → wait for deployment rollout to complete
- Flaky tests → use healer agent to fix selectors/timing

**Playwright agents for test maintenance:**

- Don't manually tweak tests - use `playwright-test-healer` to auto-fix failures
- For new features, use `playwright-test-planner` to explore and generate plans
- Use `playwright-test-generator` to create tests from plans

### Test Architecture (Flakiness Prevention)

Tests are organized into projects by execution mode:

| Project    | Claude API  | Execution           | Purpose                        |
| ---------- | ----------- | ------------------- | ------------------------------ |
| `fast`     | No (seeded) | Parallel            | UI components, seeded states   |
| `mobile`   | No (seeded) | Parallel            | Mobile viewport tests          |
| `e2e`      | Yes (real)  | Serial, shared page | Full user journey              |
| `planning` | Yes (real)  | Serial, shared page | Planning workflow (local only) |

**Key learnings from flaky test debugging:**

1. **Real Claude API tests must use shared page pattern:**

   ```typescript
   test.describe('Journey', () => {
     let sharedPage: Page;
     test.beforeAll(async ({ browser }) => {
       sharedPage = await browser.newContext().then((c) => c.newPage());
       // Set up user isolation once
     });
     test('step 1', async () => {
       /* uses sharedPage */
     });
     test('step 2', async () => {
       /* builds on step 1 */
     });
   });
   ```

2. **Never create multiple test files for real Claude API** - Each file creates new page/user, causing:
   - More API calls (slower, more flaky)
   - No shared context between tests
   - Race conditions when files run in parallel

3. **Always verify submission before waiting for response:**

   ```typescript
   await input.fill('message');
   await execButton.click();
   await expect(page.getByText('message')).toBeVisible(); // Confirms submission
   await expect(response).toBeVisible({ timeout: 60000 }); // Then wait for AI
   ```

4. **Use button click, not Enter key** - `input.press('Enter')` is flaky; use `button.click()`

5. **Wait for input to be enabled between messages:**

   ```typescript
   await expect(input).toBeEnabled({ timeout: 10000 });
   ```

6. **CI skips planning tests** - Too flaky with real Claude API. Run locally:
   ```bash
   pnpm exec playwright test --project=planning
   ```

## Key Patterns

- **Pydantic models** in `models/` shared between frontend types and backend
- **Svelte 5 runes**: `$state`, `$derived`, `$effect`, `$props`
- **API calls**: Use `$lib/api.ts`, never hardcode URLs
- **DBOS workflows**: Bump `WORKFLOW_VERSION` in `dbos_config.py` when changing workflow logic
- **HTML**: Be explicit, don't rely on browser defaults (`type="button"`, `rel="noopener"`, etc.)
- **Responsive layouts**: Use `isMobile` store to conditionally render, not CSS hide (avoids duplicate DOM elements)

## Deployment

```bash
make deploy              # Full deploy to k8s
make setup-claude-creds  # Extract Claude credentials from Keychain
```

## Documentation Philosophy

```text
README.md → docs/ → specs/ → tests/
```

Specs define behavior. Tests are the source of truth. Keep docs in sync by running planner agent after feature changes.

## Important

- Delete old code, don't keep for backward compatibility
- `make` runs from repo root
- Don't create markdown docs unless asked
- Don't commit without user review
