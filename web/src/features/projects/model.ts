/**
 * Pure rules for the projects UI: which rows the boundary keeps, how a failed call reads, how a
 * workspace request is built, and the merge-policy wording. Free of React, so each rule has a unit
 * test without a router or a query client.
 */
import { z } from 'zod';
import { ApiError, type components } from '@/api';

type Schemas = components['schemas'];
export type MergePolicy = Schemas['MergePolicy'];
export type Project = Schemas['Project'];
export type ProjectDetail = Schemas['ProjectDetail'];
export type MergePolicyView = Schemas['MergePolicyView'];
export type WorkspaceLifecycle = Schemas['WorkspaceLifecycle'];
export type PullRequest = Schemas['ProjectPRSummary'];
export type Commit = Schemas['CommitSummary'];
export type AgentKind = 'claude' | 'codex';

/** A response that breaks the contract in a way the page cannot show around, such as a bad project. */
export class ContractError extends Error {}

const text = z.string();

/*
 * Each schema lists the fields this UI reads and nothing more. A row that fails its schema is
 * dropped, so one bad row cannot blank the list or the page. Rows that pass keep their generated
 * types, so the components read the same fields the contract names.
 */
export const projectRowSchema = z.object({
  id: z.string().min(1),
  full_name: z.string().min(1),
  html_url: text,
  description: text.nullable(),
  default_branch: z.string().min(1),
  avatar_url: text.nullable(),
  open_pr_count: z.number().int().nonnegative(),
  merge_policy: z.enum(['auto', 'approval'])
});

/** The policy view is one object, not a list, so a bad one fails the section rather than being dropped. */
export const mergePolicyViewSchema = z.object({
  writes_enabled: z.boolean(),
  merge_policy: z.enum(['auto', 'approval']),
  merge_policy_version: z.number().int(),
  protected_globs: z.array(text)
});

export const pullRequestRowSchema = z.object({
  number: z.number().int(),
  title: text,
  author: text,
  created_at: text,
  url: text,
  is_mainloop: z.boolean()
});

export const commitRowSchema = z.object({
  sha: z.string().min(1),
  message: text,
  author: text,
  date: text,
  url: text
});

export const sessionRowSchema = z.object({
  id: z.string().min(1),
  status: text,
  created_at: text
});

/** Every field the workspace badges and the branch list read, so a row missing one is dropped. */
export const workspaceRowSchema = z.object({
  workspace_id: z.string().min(1),
  session_id: z.string().min(1),
  observed_state: text,
  detail: text.nullable(),
  publication_mode: z.enum(['read_only', 'branch']),
  publication_reason: text.nullable(),
  manifest: z.object({ repo_url: text, branch: text, agent_kind: text }),
  last_activity_at: text.nullable(),
  updated_at: text
});

/** The rows that pass `schema`, in their original order. Anything else is dropped silently. */
export function keepValid<T>(rows: unknown, schema: z.ZodType): T[] {
  if (!Array.isArray(rows)) return [];
  return rows.filter((row) => schema.safeParse(row).success) as T[];
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
