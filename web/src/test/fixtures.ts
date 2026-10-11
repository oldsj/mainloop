import type { components } from '@/api/schema.gen';

type Schemas = components['schemas'];

/** A workspace in the shape the backend sends, for tests that read or write the cache. */
export function workspaceFixture(
  overrides: Partial<Schemas['WorkspaceLifecycle']> = {}
): Schemas['WorkspaceLifecycle'] {
  return {
    workspace_id: 'w1',
    session_id: 's1',
    publication_mode: 'read_only',
    publication_reason: 'disabled',
    observed_state: 'running',
    detail: null,
    manifest: {
      repo_url: 'https://example.test/repo.git',
      ref: 'main',
      branch: 'main',
      depth: 1,
      agent_kind: 'codex',
      dev: { ports: [], idle_timeout_minutes: 30 },
      development_environment: null
    },
    last_activity_at: null,
    updated_at: '2026-01-01T00:00:00Z',
    ...overrides
  };
}
