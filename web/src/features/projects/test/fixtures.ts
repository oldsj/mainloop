import type { components } from '@/api/schema.gen';
import type { Session } from '@/lib/contract';

type Schemas = components['schemas'];
export type ProjectFixture = Schemas['Project'];
export type DetailFixture = Schemas['ProjectDetail'];
export type PolicyFixture = Schemas['MergePolicyView'];
export type WorkspaceFixture = Schemas['WorkspaceLifecycle'];

export const REPO_URL = 'https://github.com/acme/widgets';

export function project(overrides: Partial<ProjectFixture> = {}): ProjectFixture {
  return {
    id: 'project-a',
    user_id: 'dev-user',
    merge_policy: 'auto',
    merge_policy_version: 1,
    owner: 'acme',
    name: 'widgets',
    full_name: 'acme/widgets',
    description: 'Widget things',
    default_branch: 'main',
    avatar_url: 'https://avatars.example.com/acme.png',
    html_url: REPO_URL,
    created_at: '2026-01-01T00:00:00Z',
    last_used_at: '2026-01-02T00:00:00Z',
    metadata_updated_at: null,
    open_pr_count: 1,
    open_issue_count: 0,
    ...overrides
  };
}

export function pullRequest(overrides: Partial<Schemas['ProjectPRSummary']> = {}) {
  return {
    number: 42,
    title: 'Add the widget cache',
    state: 'open',
    author: 'octocat',
    created_at: '2026-01-03T00:00:00Z',
    updated_at: '2026-01-03T00:00:00Z',
    url: 'https://github.com/acme/widgets/pull/42',
    is_mainloop: false,
    ...overrides
  };
}

export function commit(overrides: Partial<Schemas['CommitSummary']> = {}) {
  return {
    sha: 'abcdef1234567890',
    message: 'Fix the widget lookup\n\nLong body that is not shown.',
    author: 'octocat',
    date: '2026-01-04T00:00:00Z',
    url: 'https://github.com/acme/widgets/commit/abcdef1234567890',
    ...overrides
  };
}

export function policy(overrides: Partial<PolicyFixture> = {}): PolicyFixture {
  return {
    writes_enabled: false,
    merge_policy: 'auto',
    merge_policy_version: 3,
    protected_globs: ['k8s/**', 'docs/specs/**'],
    protected_globs_version: 2,
    ...overrides
  };
}

export function workspace(overrides: Partial<WorkspaceFixture> = {}): WorkspaceFixture {
  return {
    workspace_id: 'ws-1',
    session_id: 'sess-1',
    publication_mode: 'branch',
    publication_reason: null,
    observed_state: 'running',
    detail: null,
    manifest: {
      repo_url: REPO_URL,
      ref: 'main',
      branch: 'feature/widgets',
      depth: 0,
      agent_kind: 'claude',
      dev: { ports: [], idle_timeout_minutes: 30 },
      development_environment: null
    },
    last_activity_at: null,
    updated_at: '2026-01-05T00:00:00Z',
    ...overrides
  } as WorkspaceFixture;
}

export function detail(overrides: Partial<DetailFixture> = {}): DetailFixture {
  return {
    project: project(),
    open_prs: [pullRequest()],
    recent_commits: [commit()],
    sessions: [] as Session[],
    ...overrides
  };
}
