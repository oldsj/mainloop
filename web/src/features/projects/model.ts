/**
 * Pure rules for the projects UI: which rows the boundary keeps, how a failed call reads, how a
 * workspace request is built, and the merge-policy wording. Free of React, so each rule has a unit
 * test without a router or a query client.
 */
import { z } from 'zod';
import { ApiError, type components } from '@/api';
import { contractEnum } from '@/lib/contractEnum';

type Schemas = components['schemas'];
export type MergePolicy = Schemas['MergePolicy'];
export type Project = Schemas['Project'];
export type ProjectDetail = Schemas['ProjectDetail'];
export type MergePolicyView = Schemas['MergePolicyView'];
export type WorkspaceLifecycle = Schemas['WorkspaceLifecycle'];
type PullRequest = Schemas['ProjectPRSummary'];
export type Commit = Schemas['CommitSummary'];
export type AgentKind = 'claude' | 'codex';

/** A response that breaks the contract in a way the page cannot show around, such as a bad project. */
export class ContractError extends Error {}

const text = z.string();
const MERGE_POLICIES = contractEnum<MergePolicy>()(['auto', 'approval']);
const SESSION_STATUSES = contractEnum<Schemas['SessionStatus']>()([
  'pending',
  'active',
  'idle',
  'superseded',
  'completed',
  'failed',
  'cancelled',
  'waiting_on_user'
]);
const OBSERVED_STATES = contractEnum<Schemas['WorkspaceObservedState']>()([
  'running',
  'suspending',
  'suspended',
  'resuming',
  'failed',
  'unknown'
]);
const PUBLICATION_MODES = contractEnum<WorkspaceLifecycle['publication_mode']>()([
  'read_only',
  'branch'
]);
const PUBLICATION_REASONS = contractEnum<NonNullable<WorkspaceLifecycle['publication_reason']>>()([
  'default_branch',
  'protected_branch',
  'missing_metadata',
  'no_grant',
  'disabled'
]);
const NATIVE_PROVIDERS = contractEnum<Schemas['ProviderProfile']['native_provider']>()([
  'claude',
  'codex'
]);

/*
 * Each schema checks every field the UI reads from that row, so a row that would break a read is
 * dropped and the rest still render. Each one is declared against the generated type it names, so
 * a contract change that renames or retypes a checked field fails tsc here. A field a schema does
 * not check is not guaranteed by it: a row's type is its schema's output, which lists only the
 * checked fields, and components read only those.
 */
export const projectRowSchema = z.object({
  id: z.string().min(1),
  full_name: z.string().min(1),
  html_url: text,
  description: text.nullable(),
  default_branch: z.string().min(1),
  avatar_url: text.nullable(),
  open_pr_count: z.number().int().nonnegative(),
  merge_policy: z.enum(MERGE_POLICIES)
}) satisfies z.ZodType<
  Pick<
    Project,
    | 'id'
    | 'full_name'
    | 'html_url'
    | 'description'
    | 'default_branch'
    | 'avatar_url'
    | 'open_pr_count'
    | 'merge_policy'
  >
>;
export type ProjectRow = z.output<typeof projectRowSchema>;

/** The policy view is one object, not a list, so a bad one fails the section rather than being dropped. */
export const mergePolicyViewSchema = z.object({
  writes_enabled: z.boolean(),
  merge_policy: z.enum(MERGE_POLICIES),
  merge_policy_version: z.number().int(),
  protected_globs: z.array(text)
}) satisfies z.ZodType<
  Pick<
    MergePolicyView,
    'writes_enabled' | 'merge_policy' | 'merge_policy_version' | 'protected_globs'
  >
>;

export const pullRequestRowSchema = z.object({
  number: z.number().int(),
  title: text,
  author: text,
  created_at: text,
  url: text,
  is_mainloop: z.boolean()
}) satisfies z.ZodType<
  Pick<PullRequest, 'number' | 'title' | 'author' | 'created_at' | 'url' | 'is_mainloop'>
>;
export type PullRequestRow = z.output<typeof pullRequestRowSchema>;

export const commitRowSchema = z.object({
  sha: z.string().min(1),
  message: text,
  author: text,
  date: text,
  url: text
}) satisfies z.ZodType<Commit>;
export type CommitRow = z.output<typeof commitRowSchema>;

/** Every session field the project's session rows read, so a row missing one is dropped. */
export const sessionRowSchema = z.object({
  id: z.string().min(1),
  title: text,
  status: z.enum(SESSION_STATUSES),
  needs_owner: z.boolean(),
  created_at: text,
  agent_kind: text.nullable(),
  topic: text.nullable(),
  error: text.nullable(),
  repo_url: text.nullable(),
  parent_session_id: text.nullable()
}) satisfies z.ZodType<
  Pick<
    Schemas['Session'],
    | 'id'
    | 'title'
    | 'status'
    | 'needs_owner'
    | 'created_at'
    | 'agent_kind'
    | 'topic'
    | 'error'
    | 'repo_url'
    | 'parent_session_id'
  >
>;
export type SessionRow = z.output<typeof sessionRowSchema>;

/** The provider fields a session row reads to name its native provider. */
export const providerRowSchema = z.object({
  id: z.string().min(1),
  native_provider: z.enum(NATIVE_PROVIDERS),
  aliases: z.array(text)
}) satisfies z.ZodType<Pick<Schemas['ProviderProfile'], 'id' | 'native_provider' | 'aliases'>>;

/** Every field the workspace badges and the branch list read, so a row missing one is dropped. */
export const workspaceRowSchema = z.object({
  workspace_id: z.string().min(1),
  session_id: z.string().min(1),
  observed_state: z.enum(OBSERVED_STATES),
  detail: text.nullable(),
  publication_mode: z.enum(PUBLICATION_MODES),
  publication_reason: z.enum(PUBLICATION_REASONS).nullable(),
  manifest: z.object({ repo_url: text, branch: text, agent_kind: text }),
  last_activity_at: text.nullable(),
  updated_at: text
}) satisfies z.ZodType<
  Pick<
    WorkspaceLifecycle,
    | 'workspace_id'
    | 'session_id'
    | 'observed_state'
    | 'detail'
    | 'publication_mode'
    | 'publication_reason'
    | 'last_activity_at'
    | 'updated_at'
  > & {
    manifest: Pick<Schemas['WorkspaceManifest'], 'repo_url' | 'branch' | 'agent_kind'>;
  }
>;
export type WorkspaceRow = z.output<typeof workspaceRowSchema>;

/** The rows that pass `schema`, in their original order, typed as the schema's output. Anything else is dropped silently. */
export function keepValid<T>(rows: unknown, schema: z.ZodType<T>): T[] {
  if (!Array.isArray(rows)) return [];
  return rows.flatMap((row: unknown) => {
    const parsed = schema.safeParse(row);
    return parsed.success ? [parsed.data] : [];
  });
}

/**
 * A repository in one comparable form, so `owner/name`, a GitHub URL and a `.git` URL match the
 * manifest's `repo_url`.
 */
export function repoKey(value: string): string {
  return value
    .trim()
    .toLowerCase()
    .replace(/^(https:\/\/)?(www\.)?github\.com\//, '')
    .replace(/\.git$/, '')
    .replace(/\/+$/, '');
}

/** The agent kind a workspace manifest names, or null when it names none this UI knows. */
export function agentKindOf(value: string): AgentKind | null {
  return value === 'claude' || value === 'codex' ? value : null;
}

/** The line shown for a failed call: the backend's own message when it sent one. */
export function failureText(error: unknown): string {
  if (error instanceof ApiError || error instanceof ContractError) return error.message;
  return "Couldn't reach Mainloop. Try again when it's back.";
}

/**
 * The body for `POST /workspaces`. The schema marks every field required, so this always sends the
 * server's defaults explicitly: an empty `ref` means the project's default branch, and an empty
 * `branch` lets the server choose one. Ports stay empty and the idle timeout is the server's 30.
 */
export function workspaceRequest(target: {
  projectId?: string;
  repo?: string;
  branch: string;
  agentKind: AgentKind;
}) {
  return {
    ...(target.projectId ? { project_id: target.projectId } : { repo: target.repo }),
    branch: target.branch,
    ref: '',
    depth: 0,
    dev: { ports: [], idle_timeout_minutes: 30 },
    agent_kind: target.agentKind
  };
}

export const POLICY_LABEL: Record<MergePolicy, string> = {
  auto: 'Auto',
  approval: 'Approval required'
};

export const POLICY_EXPLANATION =
  'Auto permits eligible merges after server checks. Approval requires an owner decision. Protected paths always require approval.';

/** The merge policy select is editable only when the server accepts writes and nothing is pending or stale. */
export function canSavePolicy(state: {
  draft: MergePolicy;
  saved: MergePolicy;
  writesEnabled: boolean;
  busy: boolean;
  stale: boolean;
}): boolean {
  return state.writesEnabled && !state.busy && !state.stale && state.draft !== state.saved;
}

/** First line of a commit message, for the commit list. */
export function commitTitle(message: string): string {
  return message.split('\n')[0] ?? '';
}

export function openPrText(count: number): string {
  return `${count} open ${count === 1 ? 'PR' : 'PRs'}`;
}

/** The owner's saved merge policy in words. Used by the section heading and the select. */
export function policyText(view: { merge_policy: MergePolicy }): string {
  return `Current policy: ${POLICY_LABEL[view.merge_policy]}`;
}
