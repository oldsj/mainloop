/**
 * Owner actions on a task: retry, reassign to the other provider, cancel.
 *
 * The server decides what is allowed (`view.actions`) and revalidates every submission. This
 * module only builds the request the owner saw, keeps its request ID across a lost response so a
 * resend is the same logical request, and sorts failures into what the owner should do next.
 */
import type {
  TaskActionBody,
  TaskActionKind,
  TaskAttempt,
  TaskOperation,
  TaskView,
  ProviderProfile
} from './api';
import { reasonLabel } from './taskState.ts';

export interface ActionIntent {
  kind: TaskActionKind;
  taskId: string;
  body: TaskActionBody;
}

export function newRequestId(): string {
  return `ui-${globalThis.crypto.randomUUID()}`;
}

/**
 * The request for the task exactly as the owner sees it now: its version, its current attempt
 * (null when it has none) and, for reassign, the chosen target profile.
 */
export function createIntent(
  kind: TaskActionKind,
  view: TaskView,
  options: { targetProfileId?: string; requestId?: string } = {}
): ActionIntent {
  if (kind === 'reassign' && !options.targetProfileId)
    throw new Error('Choose the provider to reassign to.');
  const body: TaskActionBody = {
    request_id: options.requestId ?? newRequestId(),
    expected_version: view.task.version,
    expected_attempt_id: view.task.current_attempt_id
  };
  if (kind === 'reassign') body.target_profile_id = options.targetProfileId;
  return { kind, taskId: view.task.id, body };
}

export interface ActionAvailability {
  enabled: boolean;
  /** Why the action is off, in words; null when it is on. */
  reason: string | null;
}

/** Disabled follows the server's eligibility; a missing entry is never assumed available. */
export function actionAvailability(
  view: TaskView,
  kind: TaskActionKind,
  options: { busy?: boolean } = {}
): ActionAvailability {
  const eligibility = view.actions[kind];
  if (!eligibility) return { enabled: false, reason: 'The server has not reported this action.' };
  if (!eligibility.available)
    return {
      enabled: false,
      reason: reasonLabel(eligibility.reason) ?? 'Not available for this task.'
    };
  if (options.busy) return { enabled: false, reason: 'Another request is in progress.' };
  return { enabled: true, reason: null };
}

/**
 * Profiles a task may be reassigned to: enabled, the other native provider, offering the
 * attempt's role, and not excluded by an owner-selected provider constraint. This is a filter for
 * the picker, not qualification: the server rejects an unqualified target on submission.
 */
export function reassignTargets(
  profiles: ProviderProfile[],
  view: TaskView,
  attempt: TaskAttempt | null
): ProviderProfile[] {
  const constraint = view.task.provider_constraint;
  return profiles.filter(
    (profile) =>
      profile.enabled &&
      profile.id !== (attempt?.profile_id ?? view.task.assigned_profile_id) &&
      (!attempt || profile.native_provider !== attempt.native_provider) &&
      (!attempt || attempt.role in profile.agents) &&
      (!constraint || constraint === profile.id)
  );
}

export type FailureKind = 'stale' | 'conflict' | 'denied' | 'not_found' | 'invalid' | 'uncertain';

export interface ActionFailure {
  kind: FailureKind;
  reason: string | null;
  message: string;
  /** The task changed (or may have): re-read it before offering the action again. */
  refresh: boolean;
  /** Whether the server may have recorded the request, so a resend must reuse the same ID. */
  resendSameRequest: boolean;
}

const REASON_MESSAGES: Record<string, string> = {
  stale_task_version:
    'The task changed since you loaded it. Review the latest state and try again.',
  stale_task_attempt:
    'The task moved to a different attempt. Review the latest state and try again.',
  request_payload_conflict:
    'That request ID was already used for a different request. Review the task and try again.',
  branch_writer_exists: 'Another attempt already owns this branch.',
  global_capacity: 'No capacity for another attempt right now.',
  parent_capacity: 'The parent task has no capacity for another attempt right now.',
  current_attempt_exists: 'The task already has a current attempt.',
  source_not_fenced: 'The current attempt has not been confirmed stopped yet.',
  fence_evidence_required: 'The current attempt has not been confirmed stopped yet.',
  task_not_found: 'Task not found.',
  inherited_provider_constraint:
    'The owner’s provider choice for this task cannot be changed here.',
  inactive_principal: 'This session is no longer authorized for the task.'
};

function providerMessage(code: string): string | undefined {
  if (code === 'provider_unavailable')
    return 'That provider is disabled or has no role for this task.';
  if (code.startsWith('provider_unqualified:'))
    return `That provider is not qualified yet (${code.slice('provider_unqualified:'.length).replaceAll('_', ' ')} is not proved).`;
  return undefined;
}

export function classifyFailure(error: unknown): ActionFailure {
  const status = (error as { status?: unknown } | null)?.status;
  const reason = (error as { reason?: unknown } | null)?.reason;
  const code = typeof reason === 'string' ? reason : null;
  const fallback = error instanceof Error ? error.message : 'The request failed.';
  const message = (code && (REASON_MESSAGES[code] ?? providerMessage(code))) || fallback;

  // No HTTP answer (offline, timeout) or a server error: the request may have been recorded.
  if (typeof status !== 'number' || status === 0 || status >= 500)
    return {
      kind: 'uncertain',
      reason: code,
      message: 'Mainloop did not confirm the request. It may have been recorded; resend to check.',
      refresh: true,
      resendSameRequest: true
    };
  if (status === 409) {
    const stale = code === 'stale_task_version' || code === 'stale_task_attempt';
    return {
      kind: stale ? 'stale' : 'conflict',
      reason: code,
      message,
      refresh: true,
      resendSameRequest: false
    };
  }
  if (status === 404)
    return { kind: 'not_found', reason: code, message, refresh: true, resendSameRequest: false };
  if (status === 401 || status === 403)
    return { kind: 'denied', reason: code, message, refresh: false, resendSameRequest: false };
  return { kind: 'invalid', reason: code, message, refresh: false, resendSameRequest: false };
}

export type ActionOutcome =
  | { status: 'accepted'; intent: ActionIntent; operation: TaskOperation }
  | { status: 'failed'; intent: ActionIntent; failure: ActionFailure }
  | { status: 'uncertain'; intent: ActionIntent; failure: ActionFailure };

type Send = (intent: ActionIntent) => Promise<TaskOperation>;

/**
 * Remembers requests whose outcome is unknown, per task and action. Submitting again while one
 * is remembered resends that exact request (same ID, same expected version and attempt), which
 * the server answers with the original operation. Anything else it sent is forgotten.
 */
export function createActionTracker(send: Send) {
  const uncertain = new Map<string, ActionIntent>();
  const inflight = new Map<string, Promise<ActionOutcome>>();
  const key = (taskId: string, kind: TaskActionKind) => `${taskId}\u0000${kind}`;

  async function run(intent: ActionIntent): Promise<ActionOutcome> {
    const k = key(intent.taskId, intent.kind);
    try {
      const operation = await send(intent);
      uncertain.delete(k);
      return { status: 'accepted', intent, operation };
    } catch (error) {
      const failure = classifyFailure(error);
      if (failure.resendSameRequest) {
        uncertain.set(k, intent);
        return { status: 'uncertain', intent, failure };
      }
      uncertain.delete(k);
      return { status: 'failed', intent, failure };
    }
  }

  return {
    /** Send a new request, or resend the remembered one for this task and action. */
    submit(
      kind: TaskActionKind,
      view: TaskView,
      options: { targetProfileId?: string } = {}
    ): Promise<ActionOutcome> {
      const k = key(view.task.id, kind);
      const running = inflight.get(k);
      if (running) return running;
      const intent = uncertain.get(k) ?? createIntent(kind, view, options);
      const promise = run(intent).finally(() => inflight.delete(k));
      inflight.set(k, promise);
      return promise;
    },
    /** The remembered request, if its outcome is still unknown. */
    pending(taskId: string, kind: TaskActionKind): ActionIntent | null {
      return uncertain.get(key(taskId, kind)) ?? null;
    },
    busy(taskId: string, kind: TaskActionKind): boolean {
      return inflight.has(key(taskId, kind));
    },
    /** Stop resending a remembered request (after the owner has seen the task's real state). */
    dismiss(taskId: string, kind: TaskActionKind): void {
      uncertain.delete(key(taskId, kind));
    }
  };
}

export type ActionTracker = ReturnType<typeof createActionTracker>;
