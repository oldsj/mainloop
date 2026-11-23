import { describe, expect, it } from 'vitest';
import { ApiError, TaskApiError } from '@/api';
import {
  agentKindOf,
  canSavePolicy,
  commitRowSchema,
  commitTitle,
  ContractError,
  failureText,
  instructionsDraft,
  instructionsStatus,
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
  workspaceRowSchema,
  parseWorkerLimit,
  workerLimitGlobalText,
  workerLimitSchema,
  workerLimitSourceText,
  workersRunningText,
  changesProvider,
  draftProvider,
  supervisorChanged,
  supervisorOptions,
  supervisorProfiles,
  supervisorRefusalText,
  supervisorSettingsSchema,
  type SupervisorSettings,
  type WorkerLimit
} from './model';
import { CLAUDE_PROFILE, CODEX_PROFILE, profile } from '@/test/fixtures/tasks';
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

describe('project instructions', () => {
  it('judges a draft as the server will: trimmed, by UTF-8 bytes', () => {
    expect(instructionsDraft('  same \n', 'same', 8192)).toEqual({
      bytes: 4,
      over: false,
      changed: false
    });
    expect(instructionsDraft('été', '', 4)).toEqual({ bytes: 5, over: true, changed: true });
    expect(instructionsDraft('', 'old', 10)).toEqual({ bytes: 0, over: false, changed: true });
  });

  it('describes none, a cleared version and a saved one', () => {
    const ago = () => '3m ago';
    expect(instructionsStatus({ version: 0, text: '', updated_at: null }, ago)).toBe(
      'No instructions yet.'
    );
    expect(instructionsStatus({ version: 4, text: '', updated_at: 'x' }, ago)).toBe(
      'Cleared (version 4, saved 3m ago).'
    );
    expect(instructionsStatus({ version: 2, text: 'Do.', updated_at: 'x' }, ago)).toBe(
      'Version 2, saved 3m ago.'
    );
  });
});

describe('the worker limit', () => {
  const limit = (overrides: Partial<WorkerLimit> = {}): WorkerLimit => ({
    project_id: 'project-a',
    max_workers: null,
    default_max_workers: 6,
    effective_max_workers: 6,
    global_max_workers: 20,
    workers_running: 2,
    ...overrides
  });

  it('counts the running workers against the limit in force', () => {
    expect(workersRunningText(limit())).toBe('2 of 6 workers running');
    expect(workersRunningText(limit({ effective_max_workers: 1, workers_running: 0 }))).toBe(
      '0 of 1 worker running'
    );
  });

  it('says whether the limit is the project’s own or the default', () => {
    expect(workerLimitSourceText(limit())).toBe('This project uses the default limit of 6.');
    expect(workerLimitSourceText(limit({ max_workers: 10, effective_max_workers: 10 }))).toBe(
      'This project’s own limit is 10. The default is 6.'
    );
  });

  it('says the owner-wide limit binds when the project asks for more than it', () => {
    // The server already caps what is in force, so the project's own value is what is compared.
    expect(workerLimitGlobalText(limit({ max_workers: 30, effective_max_workers: 20 }))).toMatch(
      /^The limit across all projects is 20, which is lower, so at most 20 run here\./
    );
    expect(
      workerLimitGlobalText(
        limit({ default_max_workers: 6, effective_max_workers: 4, global_max_workers: 4 })
      )
    ).toMatch(/^The limit across all projects is 4, which is lower/);
    expect(workerLimitGlobalText(limit())).toMatch(
      /^The limit across all projects \(20\) applies as well\./
    );
  });

  it('reads a typed limit only when it is a whole number from 1 to 100', () => {
    expect(parseWorkerLimit('1')).toBe(1);
    expect(parseWorkerLimit(' 100 ')).toBe(100);
    for (const typed of ['0', '101', '2.5', '-3', '1e2', 'six', '']) {
      expect(parseWorkerLimit(typed)).toBeNull();
    }
  });

  it('accepts the contract’s view and refuses one with a field missing', () => {
    expect(workerLimitSchema.safeParse(limit()).success).toBe(true);
    expect(workerLimitSchema.safeParse({ ...limit(), workers_running: undefined }).success).toBe(
      false
    );
  });
});

describe('the supervisor’s settings', () => {
  const settings = (overrides: Partial<SupervisorSettings> = {}): SupervisorSettings => ({
    project_id: 'project-a',
    provider_profile_id: null,
    model: null,
    effort: null,
    effective_provider_profile_id: 'claude-default',
    allowed_models: ['claude-sonnet-5', 'claude-opus-5'],
    allowed_effort_levels: ['low', 'high'],
    operation_id: null,
    ...overrides
  });
  const profiles = [CLAUDE_PROFILE, CODEX_PROFILE];
  const none = { provider_profile_id: null, model: null, effort: null };

  it('offers only profiles that are enabled and have a supervisor agent', () => {
    const list = [
      CLAUDE_PROFILE,
      profile({ id: 'off', enabled: false }),
      profile({ id: 'child-only', agents: { child: { namespace: 'agents', name: 'x' } } })
    ];
    expect(supervisorProfiles(list).map((each) => each.id)).toEqual(['claude-default']);
    expect(supervisorProfiles(undefined)).toEqual([]);
  });

  it('tells the draft’s provider only when it can be told', () => {
    // On the default: the provider in force is the default.
    expect(draftProvider(settings(), none)).toBe('claude-default');
    // A provider chosen in the form is that provider.
    expect(draftProvider(settings(), { ...none, provider_profile_id: 'codex-default' })).toBe(
      'codex-default'
    );
    // A project with its own provider does not name its default, so "default" is not known.
    expect(
      draftProvider(
        settings({
          provider_profile_id: 'codex-default',
          effective_provider_profile_id: 'codex-default'
        }),
        none
      )
    ).toBeNull();
  });

  it('lists the server’s own models for the provider in force, by the profile’s labels', () => {
    const options = supervisorOptions(settings(), none, profiles);
    expect(options.models).toEqual([
      { id: 'claude-sonnet-5', label: 'Sonnet 5' },
      { id: 'claude-opus-5', label: 'Opus 5' }
    ]);
    // The view's levels, not the profile's longer list: they are what a save is checked against.
    expect(options.efforts).toEqual(['low', 'high']);
  });

  it('lists another provider’s profile before it is saved, and keeps a choice it lacks, marked', () => {
    const draft = { provider_profile_id: 'codex-default', model: 'claude-opus-5', effort: null };
    const options = supervisorOptions(settings(), draft, profiles);
    expect(options.models).toEqual([
      ...CODEX_PROFILE.models.map((model) => ({ id: model.id, label: model.label })),
      { id: 'claude-opus-5', label: 'claude-opus-5 (not offered)' }
    ]);
    expect(options.efforts).toEqual(CODEX_PROFILE.effort_levels);
  });

  it('says a change needs saving, and when it would restart the supervisor', () => {
    const view = settings();
    expect(supervisorChanged(view, none)).toBe(false);
    expect(supervisorChanged(view, { ...none, effort: 'high' })).toBe(true);
    expect(changesProvider(view, { ...none, effort: 'high' })).toBe(false);
    // Naming the provider already in force changes the choice but not the provider.
    expect(changesProvider(view, { ...none, provider_profile_id: 'claude-default' })).toBe(false);
    expect(changesProvider(view, { ...none, provider_profile_id: 'codex-default' })).toBe(true);
    // Back to a default that cannot be told: assume it moves.
    expect(
      changesProvider(
        settings({
          provider_profile_id: 'codex-default',
          effective_provider_profile_id: 'codex-default'
        }),
        none
      )
    ).toBe(true);
  });

  it('puts a refusal in words by its reason, and names the hourly limit', () => {
    const refused = (detail: { reason: string; limit?: number }) =>
      new TaskApiError(detail.reason, 409, detail.reason, detail as never);
    expect(supervisorRefusalText(refused({ reason: 'task_settling' }))).toBe(
      'The supervisor is still starting or stopping. Try again in a moment.'
    );
    expect(supervisorRefusalText(refused({ reason: 'handoff_limit', limit: 3 }))).toBe(
      'The supervisor has been restarted 3 times in the last hour, which is the limit. Try again later.'
    );
    expect(supervisorRefusalText(refused({ reason: 'handoff_limit' }))).toMatch(
      /as often as it may be in an hour/
    );
    // A reason this page does not know falls back to what the server sent.
    expect(supervisorRefusalText(refused({ reason: 'something_new' }))).toBe('something_new');
    expect(supervisorRefusalText(new Error('offline'))).toMatch(/Couldn.t reach Mainloop/);
  });

  it('accepts the contract’s view and refuses one with a field missing', () => {
    expect(supervisorSettingsSchema.safeParse(settings()).success).toBe(true);
    expect(
      supervisorSettingsSchema.safeParse({ ...settings(), allowed_models: undefined }).success
    ).toBe(false);
  });
});
