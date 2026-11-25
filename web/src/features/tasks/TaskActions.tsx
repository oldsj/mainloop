import { useQueryClient } from '@tanstack/react-query';
import { Link } from '@tanstack/react-router';
import { useState } from 'react';
import { ConfirmDialog } from '@/components/shared/confirm-dialog';
import { Button } from '@/components/ui/button';
import type { ProviderProfile, TaskActionKind, TaskView } from '@/lib/contract';
import {
  actionAvailability,
  reassignTargets,
  type ActionIntent,
  type ActionOutcome,
  type ActionTracker
} from '@/lib/taskActions';
import { currentAttempt, switchBlockers } from '@/lib/taskState';
import {
  actionNotice,
  askAbout,
  FIXED_RETRY,
  resendSummary,
  waitsOnOwner,
  type Notice
} from './model';
import { rereadTask } from './queries';

const KINDS: readonly TaskActionKind[] = ['retry', 'reassign', 'cancel'];

const LABELS: Record<TaskActionKind, string> = {
  retry: 'Retry',
  reassign: 'Reassign',
  cancel: 'Cancel task'
};

const NOTICE_CLASS: Record<Notice['tone'], string> = {
  info: 'text-term-green',
  warn: 'text-term-yellow',
  error: 'text-destructive'
};

function rememberedFrom(
  tracker: ActionTracker,
  taskId: string
): Partial<Record<TaskActionKind, ActionIntent>> {
  const found: Partial<Record<TaskActionKind, ActionIntent>> = {};
  for (const kind of KINDS) {
    const intent = tracker.pending(taskId, kind);
    if (intent) found[kind] = intent;
  }
  return found;
}

interface TaskActionsProps {
  view: TaskView;
  tracker: ActionTracker;
  profiles: ProviderProfile[] | undefined;
  profilesError: unknown;
  offline: boolean;
  /**
   * Puts a message about this task in the main chat's composer, unsent. Offered on a task that
   * waits on the owner, as the way to say what was done about it.
   */
  onAsk?: (text: string) => void;
}

/**
 * Owner actions for one task. The server decides what is allowed (`view.actions`) and checks
 * every submission; this panel only sends what the owner saw. A request whose outcome is unknown
 * stays as a resend with its original version, attempt and target until the owner resends or
 * discards it.
 *
 * The actions that move the task on come first: Retry when the server offers it, and on a task that
 * waits on the owner, the two ways to answer in the main chat. Cancel is last and quiet; it turns
 * red only in its confirmation.
 */
export function TaskActions({
  view,
  tracker,
  profiles,
  profilesError,
  offline,
  onAsk
}: TaskActionsProps) {
  const queryClient = useQueryClient();
  const taskId = view.task.id;
  const [notice, setNotice] = useState<Notice | null>(null);
  const [remembered, setRemembered] = useState(() => rememberedFrom(tracker, taskId));
  const [sending, setSending] = useState<TaskActionKind | null>(null);
  const [target, setTarget] = useState('');
  // The task as the owner saw it when the cancel dialog opened. The request is built from this, not
  // from the live view, so a change that lands while the dialog is open cannot retarget it.
  const [cancelSeen, setCancelSeen] = useState<TaskView | null>(null);
  // Set when the task moved on before the owner confirmed. It stays until the owner acts again.
  const [cancelChanged, setCancelChanged] = useState(false);

  const targets = reassignTargets(profiles ?? [], view, currentAttempt(view));
  // Derived, not synced: the first target stands in until the owner picks another.
  const selected = targets.some((profile) => profile.id === target)
    ? target
    : (targets[0]?.id ?? '');
  const blockers = switchBlockers(view);

  async function submit(
    kind: TaskActionKind,
    options: { targetProfileId?: string } = {},
    seen: TaskView = view
  ) {
    if (tracker.busy(taskId, kind)) return;
    setNotice(null);
    setCancelChanged(false);
    setSending(kind);
    let outcome: ActionOutcome;
    try {
      outcome = await tracker.submit(kind, seen, options);
    } catch (error) {
      // Building the request failed before anything was sent (for example, no target chosen).
      setNotice({
        tone: 'error',
        text: error instanceof Error ? error.message : 'The request could not be built.',
        reason: null
      });
      setSending(null);
      return;
    }
    // A settled action, or a refusal that means the owner is looking at a stale task, re-reads
    // the task before the notice says anything, so the owner never acts on the old state again.
    if (outcome.status === 'accepted' || outcome.failure.refresh) {
      await rereadTask(queryClient, taskId);
    }
    setRemembered(rememberedFrom(tracker, taskId));
    setNotice(actionNotice(outcome));
    setSending(null);
  }

  function discard(kind: TaskActionKind) {
    tracker.dismiss(taskId, kind);
    setRemembered(rememberedFrom(tracker, taskId));
    setNotice({
      tone: 'info',
      text: 'Unconfirmed request discarded. Check the task before acting again.',
      reason: null
    });
  }

  /** Enabled only when the server allows it, nothing is in flight, and no unconfirmed request is held. */
  function gate(kind: TaskActionKind): { enabled: boolean; reason: string | null } {
    if (offline) return { enabled: false, reason: 'Mainloop is offline.' };
    if (remembered[kind]) {
      return { enabled: false, reason: 'Resend or discard the unconfirmed request first.' };
    }
    return actionAvailability(view, kind, { busy: sending !== null });
  }

  const retry = gate('retry');
  const cancel = gate('cancel');
  // A project's standing supervisor is never retried from scratch, cancelled or handed to another
  // provider: the server's retry on it is a restart (a new session on the same task, its workers
  // kept), and it refuses the other two for good. So Retry is named for what it does there, and
  // the two controls that can never work are not shown.
  const standing = view.task.standing === true;
  const retryLabel = standing ? 'Restart' : LABELS.retry;
  // A code task's retry and reassign need a GitHub App to read the branch. While the server reports them
  // unavailable, the owner sees no control for them and no note about why: a control that cannot work is not shown,
  // and how the server is built is not the owner's concern.
  const retryHidden = view.actions.retry?.reason === 'handoff_unavailable';
  // Retry starts a new attempt. That is the next step for a task that has stopped (blocked or
  // failed), so there it leads. On a task that is running, or waiting on CI or on its pull request,
  // it is still offered but is not what the page points at.
  const retryLeads =
    retry.enabled && (view.task.status === 'blocked' || view.task.status === 'failed');
  const reassignHidden =
    view.actions.reassign?.reason === 'handoff_unavailable' ||
    view.actions.reassign?.reason === 'standing_supervisor';
  // The server refuses cancel with this reason once the task has ended. There is nothing left to
  // cancel then, so the button and its reason are not shown, like the two above.
  const cancelHidden =
    view.actions.cancel?.reason === 'cancel_unavailable' ||
    view.actions.cancel?.reason === 'standing_supervisor';
  // On a task that waits on the owner, the main chat is where they say what they did about it. When
  // the server's own Retry works, that is the direct way, so the chat's "retry" is not offered too.
  const answerInChat = onAsk !== undefined && waitsOnOwner(view);
  const sessionId = currentAttempt(view)?.session_id ?? null;
  // The dialog closes, sending nothing, once the task has moved on from what the owner confirmed:
  // a new version or attempt, or cancel no longer offered. The owner then re-opens it on the new task.
  const cancelMoved =
    cancelSeen !== null &&
    (cancelSeen.task.version !== view.task.version ||
      currentAttempt(cancelSeen)?.id !== currentAttempt(view)?.id ||
      !actionAvailability(view, 'cancel', { busy: false }).enabled);
  const cancelOpen = cancelSeen !== null && !cancelMoved;
  // The dialog's target is dropped once the task has moved on; only the status line is kept. Our own
  // cancel also moves the task, so the check waits until no action is in flight.
  if (cancelMoved && sending === null) {
    setCancelSeen(null);
    setCancelChanged(true);
  }
  const reassignGate = gate('reassign');
  const reassign =
    reassignGate.enabled && selected !== ''
      ? { enabled: true, reason: null }
      : {
          enabled: false,
          reason:
            reassignGate.reason ??
            (profilesError
              ? `Could not load providers: ${profilesError instanceof Error ? profilesError.message : 'unknown error'}.`
              : profiles === undefined
                ? 'Loading providers…'
                : 'No other provider is available.')
        };

  // An ended task with no action left, nothing unconfirmed and nothing to report shows no panel.
  const nothingToShow =
    retryHidden &&
    reassignHidden &&
    cancelHidden &&
    !answerInChat &&
    blockers.length === 0 &&
    notice === null &&
    !cancelChanged &&
    KINDS.every((kind) => !remembered[kind]);
  if (nothingToShow) return null;

  return (
    <section
      aria-labelledby="actions-heading"
      data-testid="task-actions"
      className="gap-sm p-md border-border-subtle flex flex-col border-t"
    >
      <h2 id="actions-heading" className="sr-only">
        Actions
      </h2>

      {blockers.length > 0 ? (
        <div data-testid="task-blockers" className="gap-xs text-body flex flex-col">
          <p className="font-medium">Blocking a provider switch:</p>
          <ul className="gap-xs pl-lg flex list-disc flex-col">
            {blockers.map((blocker) => (
              <li key={blocker} className="break-words">
                {blocker}
              </li>
            ))}
          </ul>
        </div>
      ) : null}

      {KINDS.map((kind) => {
        const intent = remembered[kind];
        return intent ? (
          <section
            key={kind}
            data-testid="task-resend"
            aria-label={`Unconfirmed ${kind} request`}
            className="gap-sm border-term-yellow/50 p-md flex flex-col border"
          >
            <p className="text-body break-words">{resendSummary(intent)}</p>
            <p className="text-muted-foreground text-caption">
              Mainloop did not confirm this request. Resending reuses its request ID, so the server
              answers the original request rather than making a second one.
            </p>
            <div className="gap-sm flex flex-wrap">
              <Button
                type="button"
                variant="outline"
                disabled={sending === kind || offline}
                onClick={() => void submit(kind)}
              >
                Resend original request
              </Button>
              <Button
                type="button"
                variant="ghost"
                disabled={sending === kind}
                onClick={() => discard(kind)}
              >
                Discard
              </Button>
            </div>
            {offline ? (
              <p className="text-muted-foreground text-caption">Mainloop is offline.</p>
            ) : null}
            {sending === kind ? (
              <p role="status" className="text-muted-foreground text-caption">
                Sending request…
              </p>
            ) : null}
          </section>
        ) : null;
      })}

      <div className="gap-sm flex flex-wrap items-center">
        {retryHidden ? null : (
          <Button
            type="button"
            variant={retryLeads ? 'default' : 'outline'}
            data-testid="task-action-retry"
            disabled={!retry.enabled}
            onClick={() => void submit('retry')}
          >
            {retryLabel}
          </Button>
        )}
        {answerInChat && (retryHidden || !retry.enabled) ? (
          <Button
            type="button"
            data-testid="task-action-fixed"
            onClick={() => onAsk(askAbout(view, FIXED_RETRY))}
          >
            I fixed it, retry
          </Button>
        ) : null}
        {answerInChat ? (
          <Button
            type="button"
            variant="outline"
            data-testid="task-action-answer"
            onClick={() => onAsk(askAbout(view))}
          >
            Answer in chat
          </Button>
        ) : null}
        {answerInChat && sessionId ? (
          <Button asChild variant="outline">
            <Link to="/sessions/$id" params={{ id: sessionId }} data-testid="task-action-session">
              Open session
            </Link>
          </Button>
        ) : null}
        {cancelHidden ? null : (
          <Button
            type="button"
            variant="outline"
            data-testid="task-action-cancel"
            disabled={!cancel.enabled}
            onClick={() => {
              setCancelSeen(view);
              setCancelChanged(false);
            }}
            className="text-destructive"
          >
            {LABELS.cancel}
          </Button>
        )}
      </div>

      {answerInChat ? (
        <p className="text-muted-foreground text-caption">
          The first two open the main chat with a message about this task, ready for you to send.
        </p>
      ) : null}
      {/* A disabled action keeps its reason on screen, named, so it is read with the button. */}
      {standing && !retryHidden ? (
        <p className="text-muted-foreground text-caption" data-testid="task-restart-note">
          Restart gives the supervisor a new session on this same task. Its workers keep running.
        </p>
      ) : null}
      {!retryHidden && retry.reason ? (
        <p className="text-muted-foreground text-caption">
          {retryLabel}: {retry.reason}
        </p>
      ) : null}
      {!cancelHidden && cancel.reason ? (
        <p className="text-muted-foreground text-caption">
          {LABELS.cancel}: {cancel.reason}
        </p>
      ) : null}
      {cancelChanged ? (
        <p role="status" className="text-muted-foreground text-caption">
          The task changed before you confirmed, so nothing was cancelled. Check it, then cancel
          again.
        </p>
      ) : null}

      {reassignHidden ? null : targets.length === 0 && profiles !== undefined && !profilesError ? (
        // Nobody to hand the task to: one line says so, in place of an empty picker.
        <p className="text-muted-foreground text-caption" data-testid="task-reassign-none">
          {LABELS.reassign}: {reassign.reason}
        </p>
      ) : (
        <div className="gap-xs flex flex-col">
          <label htmlFor="reassign-target" className="text-body font-medium">
            Provider to reassign to
          </label>
          <div className="gap-sm flex flex-wrap items-center">
            <select
              id="reassign-target"
              value={selected}
              disabled={targets.length === 0 || sending !== null || !reassignGate.enabled}
              onChange={(event) => setTarget(event.target.value)}
              className="border-border text-foreground bg-background px-md min-h-control text-field rounded-none border"
            >
              {targets.map((profile) => (
                <option key={profile.id} value={profile.id}>
                  {profile.display_name} ({profile.native_provider})
                </option>
              ))}
            </select>
            <Button
              type="button"
              variant="outline"
              data-testid="task-action-reassign"
              disabled={!reassign.enabled}
              onClick={() => void submit('reassign', { targetProfileId: selected })}
            >
              {LABELS.reassign}
            </Button>
          </div>
          {reassign.reason ? (
            <p className="text-muted-foreground text-caption">{reassign.reason}</p>
          ) : null}
        </div>
      )}

      {notice ? (
        <div
          role={notice.tone === 'info' ? 'status' : 'alert'}
          data-testid="task-action-notice"
          className={`gap-xs text-body flex flex-col ${NOTICE_CLASS[notice.tone]}`}
        >
          <p className="break-words">{notice.text}</p>
          {notice.reason ? (
            <p className="text-caption">
              Server reason: <code>{notice.reason}</code>
            </p>
          ) : null}
          <div>
            <Button type="button" variant="ghost" size="sm" onClick={() => setNotice(null)}>
              Dismiss
            </Button>
          </div>
        </div>
      ) : null}

      <ConfirmDialog
        open={cancelOpen}
        onOpenChange={(open) => {
          if (!open) setCancelSeen(null);
        }}
        title="Cancel this task?"
        description="Cancellation is a durable operation. The current attempt is stopped, and the task keeps its capacity until the server confirms the stop."
        confirmLabel="Confirm cancel"
        cancelLabel="Keep running"
        destructive
        pending={sending === 'cancel'}
        onConfirm={() => {
          if (cancelSeen === null) return;
          void submit('cancel', {}, cancelSeen).then(() => setCancelSeen(null));
        }}
      />
    </section>
  );
}
