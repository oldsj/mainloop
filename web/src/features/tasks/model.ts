/**
 * Pure helpers for the task UI: how a failed read is described, what an action outcome says, and
 * the wording shared by the page and the tree. Components import from here so the copy lives in
 * one place.
 */
import type { TaskProjection, TaskView } from '@/lib/contract';
import type { ActionIntent, ActionOutcome } from '@/lib/taskActions';
import { httpsUrl } from '@/lib/url';

export type NoticeTone = 'info' | 'warn' | 'error';

export interface Notice {
  tone: NoticeTone;
  text: string;
  /** The backend's stable code, shown beside the message so the owner can quote it. */
  reason: string | null;
}

export type LoadFailure = 'not-found' | 'unreachable' | 'server' | 'other';

/** Read failures in the words the owner needs: not found, no connection, server error, other. */
export function taskLoadFailure(error: unknown): LoadFailure {
  const status = (error as { status?: unknown } | null)?.status;
  if (typeof status !== 'number' || status === 0) return 'unreachable';
  if (status === 404) return 'not-found';
  if (status >= 500) return 'server';
  return 'other';
}

const capitalise = (text: string) => text.charAt(0).toUpperCase() + text.slice(1);

/** What the owner sees after an action settles. The server's own message is kept for failures. */
export function actionNotice(outcome: ActionOutcome): Notice {
  if (outcome.status === 'accepted') {
    return {
      tone: 'info',
      text: `${capitalise(outcome.operation.kind)} requested. Progress shows below as the server reports it.`,
      reason: null
    };
  }
  return {
    tone: outcome.status === 'uncertain' ? 'warn' : 'error',
    text: outcome.failure.message,
    reason: outcome.failure.reason
  };
}

/** The request a lost reply left behind, in the owner's words: what it asked for, and when. */
export function resendSummary(intent: ActionIntent): string {
  const attempt = intent.body.expected_attempt_id ?? 'none';
  const target = intent.kind === 'reassign' ? intent.body.target_profile_id : 'none';
  return `Original ${intent.kind}: version ${intent.body.expected_version}, attempt ${attempt}, target ${target}.`;
}

/**
 * The start of a message to Mainloop about this task, in the form the explorer's "Ask Mainloop"
 * uses, so the main thread reads both the same way. `say` is the sentence that follows.
 */
export function askAbout(view: TaskView, say = ''): string {
  const title = view.task.title.trim() || 'untitled';
  return `About task ${title} (${view.task.id.slice(0, 8)}): ${say}`;
}

/** What "I fixed it, retry" puts in the composer. It is not sent until the owner sends it. */
export const FIXED_RETRY = 'I fixed it. Retry this task.';

/**
 * Whether the task is stopped on something only the owner can clear: an open decision, or a
 * blocked or failed task. Such a task leads with the ways to clear it.
 */
export function waitsOnOwner(view: TaskView): boolean {
  const { status, owner_decision } = view.task;
  return owner_decision != null || status === 'blocked' || status === 'failed';
}

/** What a task that waits on publication needs, in the owner's words, and its pull request to open. */
export interface PublicationAsk {
  /** Which case this is, for tests and for the banner's tone. */
  kind: 'ready' | 'merged' | 'closed' | 'none';
  /** True when only the owner can move it: the banner then leads as "Needs you". */
  needsOwner: boolean;
  text: string;
  prNumber: number | null;
  prUrl: string | null;
}

/**
 * A code task whose agent reported `completed` waits on publication: it completes only when its
 * own pull request is merged by a merge its own session proposed (docs/specs/tasks.md). The page
 * said only "awaiting publication". This says where the pull request stands and what moves the
 * task on, for the states where nothing more happens on its own:
 *
 * - open with CI passed and no merge proposed: someone has to ask for the merge;
 * - merged: complete within a minute if the task's own merge did it, else never;
 * - closed without merging, or no pull request at all: nothing is left to merge.
 *
 * While CI is still running or failed, or a merge is under way, there is nothing to ask: null.
 */
export function publicationAsk(view: TaskView): PublicationAsk | null {
  if (view.task.status !== 'waiting' || view.task.reason !== 'publication') return null;
  const p = view.projection;
  const prNumber = p.pr_number ?? null;
  const pr = prNumber != null ? `PR #${prNumber}` : 'The pull request';
  // The link is the server's; only an https URL is offered as one.
  const base = { prNumber, prUrl: httpsUrl(p.pr_url) };
  if (p.pr_state === 'merged') {
    return {
      ...base,
      kind: 'merged',
      needsOwner: false,
      text: `${pr} is merged. If this task's own agent merged it, the task completes within a minute. If it was merged another way (on GitHub, or by another session), the task cannot complete on its own: cancel it to close it.`
    };
  }
  if (p.pr_state === 'closed') {
    return {
      ...base,
      kind: 'closed',
      needsOwner: false,
      text: `${pr} was closed without merging, so this task will keep waiting. Ask its agent in a follow-up to open a new one, or cancel the task.`
    };
  }
  if (p.pr_state === 'open') {
    if (p.ci_state !== 'success' || p.merge_proposal_id) return null;
    return {
      ...base,
      kind: 'ready',
      needsOwner: true,
      text: `${pr} is open and CI passed. Nothing merges it on its own. Ask this task's agent in a follow-up to merge it, so the task completes when the merge lands, or cancel the task if it should not land.`
    };
  }
  if (prNumber != null || p.pr_url) return null;
  return {
    ...base,
    kind: 'none',
    needsOwner: false,
    text: 'The agent reported this task finished, but no pull request is linked to it. A code task completes when its own pull request is merged, so this one will keep waiting. Ask its agent in a follow-up to open one, or cancel the task if there is nothing to publish.'
  };
}

/**
 * Whether the task has anything to say about a pull request: a repository or branch to publish
 * from, or a publication fact the server has reported. A task with neither (an ops task, say) has no pull
 * request section at all.
 */
export function hasPublication(view: TaskView): boolean {
  const p = view.projection;
  return Boolean(
    p.repository ||
    view.task.checkout?.branch ||
    p.pr_number != null ||
    p.pr_url ||
    (p.pr_state ?? 'unknown') !== 'unknown' ||
    p.merge_state ||
    p.pr_description
  );
}

/** The list's line for rows this page cannot read. Such a row is not shown, and the line says so. */
export function unreadableTasksText(count: number): string {
  return count === 1
    ? '1 task could not be read and is not shown.'
    : `${count} tasks could not be read and are not shown.`;
}

/**
 * True while the task has no pull request. Publication has not happened, so the PR, CI and merge
 * facts have nothing to describe yet. `publication_state` is a publication mode, not a status, so
 * it does not decide this.
 */
export function notPublishedYet(projection: Partial<TaskProjection> | undefined): boolean {
  const p = projection ?? {};
  return p.pr_number == null && !p.pr_url && (p.pr_state ?? 'unknown') === 'unknown';
}
