/**
 * Pure helpers for the task UI: how a failed read is described, what an action outcome says, and
 * the wording shared by the page and the tree. Components import from here so the copy lives in
 * one place.
 */
import type { TaskView } from '@/lib/contract';
import type { ActionIntent, ActionOutcome } from '@/lib/taskActions';
import { currentAttempt } from '@/lib/taskState';

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

/** The version and attempt a new action will act on, so the owner knows what they are changing. */
export function actingOn(view: TaskView): string {
  const attempt = currentAttempt(view);
  const at = attempt ? `, attempt ${attempt.number}` : ', no attempt yet';
  return `Acting on version ${view.task.version}${at}.`;
}

/** Depth 0 is flush; depth 1 and 2 step in; anything deeper stops at the last step. */
export function indentClass(depth: number): string {
  if (depth <= 0) return '';
  if (depth === 1) return 'pl-md';
  if (depth === 2) return 'pl-lg';
  return 'pl-xl';
}
