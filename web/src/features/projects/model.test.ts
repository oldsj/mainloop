import { describe, expect, it } from 'vitest';
import { ApiError } from '@/api';
import {
  agentKindOf,
  canSavePolicy,
  commitRowSchema,
  commitTitle,
  ContractError,
  failureText,
  keepValid,
  mergePolicyViewSchema,
  openPrText,
  POLICY_EXPLANATION,
  POLICY_LABEL,
  policyText,
  projectRowSchema,
  providerRowSchema,
  pullRequestRowSchema,
  sessionRowSchema,
  workspaceRowSchema
} from './model';
import { commit, policy, project, pullRequest, workspace } from './test/fixtures';

describe('boundary schemas', () => {
  it('accepts a project row, including a null description and avatar', () => {
    expect(projectRowSchema.safeParse(project()).success).toBe(true);
    expect(
      projectRowSchema.safeParse(project({ description: null, avatar_url: null })).success
    ).toBe(true);
  });

  it('accepts a project row whose default branch is still empty', () => {
    expect(projectRowSchema.safeParse(project({ default_branch: '' })).success).toBe(true);
  });

  it('rejects a project row with an unknown merge policy or an empty id', () => {
    expect(projectRowSchema.safeParse({ ...project(), merge_policy: 'mystery' }).success).toBe(
      false
    );
    expect(projectRowSchema.safeParse({ ...project(), id: '' }).success).toBe(false);
    expect(projectRowSchema.safeParse({ ...project(), open_pr_count: -1 }).success).toBe(false);
  });

  it('accepts the merge policy view only with a whole-number version and a list of globs', () => {
    expect(mergePolicyViewSchema.safeParse(policy()).success).toBe(true);
    expect(
      mergePolicyViewSchema.safeParse({ ...policy(), merge_policy_version: 1.5 }).success
    ).toBe(false);
    expect(
      mergePolicyViewSchema.safeParse({ ...policy(), protected_globs: 'k8s/**' }).success
    ).toBe(false);
  });

  it('accepts pull request and commit rows and rejects a wrong number or sha type', () => {
    expect(pullRequestRowSchema.safeParse(pullRequest()).success).toBe(true);
    expect(
      pullRequestRowSchema.safeParse({ ...pullRequest(), number: 'not-a-number' }).success
    ).toBe(false);
    expect(commitRowSchema.safeParse(commit()).success).toBe(true);
    expect(commitRowSchema.safeParse({ ...commit(), sha: 123 }).success).toBe(false);
  });

  it('checks every session field the list reads, and drops a row that would break a read', () => {
    const row = {
      id: 's1',
      title: 'Fix the build',
      status: 'active',
      needs_owner: false,
      created_at: '2026-01-01T00:00:00Z',
      agent_kind: 'claude',
      topic: null,
      error: null,
      repo_url: 'https://github.com/owner/repo',
      parent_session_id: null
    };
    expect(sessionRowSchema.safeParse(row).success).toBe(true);
    expect(sessionRowSchema.safeParse({ ...row, id: '' }).success).toBe(false);
    expect(sessionRowSchema.safeParse({ ...row, status: 'running' }).success).toBe(false);
    expect(sessionRowSchema.safeParse({ ...row, needs_owner: 'no' }).success).toBe(false);
    expect(sessionRowSchema.safeParse({ ...row, repo_url: 42 }).success).toBe(false);
    const withoutRepo: Record<string, unknown> = { ...row };
    delete withoutRepo.repo_url;
    expect(sessionRowSchema.safeParse(withoutRepo).success).toBe(false);
  });

  it('checks the provider fields the session row reads', () => {
    const row = { id: 'claude-main', native_provider: 'claude', aliases: ['opus'] };
    expect(providerRowSchema.safeParse(row).success).toBe(true);
    expect(providerRowSchema.safeParse({ ...row, native_provider: 'gemini' }).success).toBe(false);
    expect(providerRowSchema.safeParse({ ...row, aliases: [7] }).success).toBe(false);
  });

  it('needs the publication mode, the manifest repository, branch and agent to show a workspace', () => {
    expect(workspaceRowSchema.safeParse(workspace()).success).toBe(true);
    expect(workspaceRowSchema.safeParse({ ...workspace(), publication_mode: 'push' }).success).toBe(
      false
    );
    const noMode: Record<string, unknown> = { ...workspace() };
    delete noMode.publication_mode;
    expect(workspaceRowSchema.safeParse(noMode).success).toBe(false);
    expect(
      workspaceRowSchema.safeParse({
        ...workspace(),
        manifest: { repo_url: 'https://github.com/acme/widgets', branch: 'main' }
      }).success
    ).toBe(false);
  });
});

describe('keepValid', () => {
  it('keeps the rows that pass, in their original order, and drops the rest', () => {
    const rows = [
      project({ id: 'a' }),
      { ...project({ id: 'b' }), merge_policy: 'mystery' },
      project({ id: 'c' })
    ];
    const kept = keepValid<{ id: string }>(rows, projectRowSchema);
    expect(kept.map((row) => row.id)).toEqual(['a', 'c']);
  });

  it('returns an empty list when the payload is not a list at all', () => {
    expect(keepValid(null, projectRowSchema)).toEqual([]);
    expect(keepValid({ id: 'a' }, projectRowSchema)).toEqual([]);
  });
});

describe('agentKindOf', () => {
  it('names only the two agents the UI knows', () => {
    expect(agentKindOf('claude')).toBe('claude');
    expect(agentKindOf('codex')).toBe('codex');
    expect(agentKindOf('gemini')).toBeNull();
    expect(agentKindOf('')).toBeNull();
  });
});

describe('failureText', () => {
  it('shows the backend message for an API or contract error', () => {
    expect(failureText(new ApiError('Repository is not accessible', 403))).toBe(
      'Repository is not accessible'
    );
    expect(
      failureText(new ContractError('Mainloop sent a project that does not match the contract.'))
    ).toBe('Mainloop sent a project that does not match the contract.');
  });

  it('gives a plain message when the request never got a reply', () => {
    expect(failureText(new TypeError('Failed to fetch'))).toBe(
      "Couldn't reach Mainloop. Try again when it's back."
    );
    expect(failureText(undefined)).toBe("Couldn't reach Mainloop. Try again when it's back.");
  });
});

describe('merge policy wording and save rule', () => {
  it('labels the two policies and reads the saved one in words', () => {
    expect(POLICY_LABEL).toEqual({ auto: 'Auto', approval: 'Approval required' });
    expect(policyText({ merge_policy: 'auto' })).toBe('Current policy: Auto');
    expect(policyText({ merge_policy: 'approval' })).toBe('Current policy: Approval required');
    expect(POLICY_EXPLANATION).toMatch(/^Auto permits eligible merges/);
    expect(POLICY_EXPLANATION).toMatch(/Protected paths always require approval\.$/);
  });

  it('saves only a changed policy, with writes on and nothing pending or stale', () => {
    const base = {
      draft: 'approval' as const,
      saved: 'auto' as const,
      writesEnabled: true,
      busy: false,
      stale: false
    };
    expect(canSavePolicy(base)).toBe(true);
    expect(canSavePolicy({ ...base, draft: 'auto' })).toBe(false);
    expect(canSavePolicy({ ...base, writesEnabled: false })).toBe(false);
    expect(canSavePolicy({ ...base, busy: true })).toBe(false);
    expect(canSavePolicy({ ...base, stale: true })).toBe(false);
  });
});

describe('list and row helpers', () => {
  it('shows the first line of a commit message only', () => {
    expect(commitTitle('Fix the widget lookup\n\nLong body')).toBe('Fix the widget lookup');
    expect(commitTitle('')).toBe('');
  });

  it('counts open pull requests with the right singular or plural', () => {
    expect(openPrText(0)).toBe('0 open PRs');
    expect(openPrText(1)).toBe('1 open PR');
    expect(openPrText(3)).toBe('3 open PRs');
  });
});
