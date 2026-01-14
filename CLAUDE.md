# CLAUDE.md

## Project Overview

**mainloop** is an attention management system - one place to focus, accessible from any device.

- One continuous conversation that persists across devices
- Sessions handle background work with inline threading in main conversation
- Notifications surface when sessions need attention or complete

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

**Prerequisites:** `brew install devspace kind`

```bash
make dev          # Start DevSpace with hot reload (frontend :5173, backend :8081)
make dev-stop     # Stop DevSpace
make dev-reset    # Reset database
make fmt          # Format before committing
```

DevSpace syncs files directly to containers - code changes appear in <2s without rebuilding images.

**Backend:** `uv add <package>` for dependencies (never edit pyproject.toml manually)

**Frontend:** `pnpm add <package>` for dependencies

## Testing

```bash
make test         # Deploy to Kind + open Playwright UI
make test-run     # Run tests headless
make test-reset   # Clear DB + namespaces between runs
```

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

- **Svelte stores + HMR gotcha**: When editing store files, Vite HMR can leave stale module references - components keep old store imports while the store file gets new code. Hard refresh doesn't fix this. Solution: touch (add a comment to) the component that imports the store to force Vite to recompile it with fresh imports. If derived stores don't update after their dependencies change, this is almost always the cause.
- **Pydantic models** in `models/` shared between frontend types and backend
- **Svelte 5 runes**: `$state`, `$derived`, `$effect`, `$props`
- **API calls**: Use `$lib/api.ts`, never hardcode URLs
- **DBOS workflows**: Bump `WORKFLOW_VERSION` in `dbos_config.py` when changing workflow logic
- **HTML**: Be explicit, don't rely on browser defaults (`type="button"`, `rel="noopener"`, etc.)
- **Responsive layouts**: Use `isMobile` store to conditionally render, not CSS hide (avoids duplicate DOM elements)
- **K8s scripts**: Always use explicit `--context kind-${KIND_CLUSTER_NAME:-mainloop-test}` in kubectl commands to avoid targeting wrong cluster

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
