import { useQueryClient } from '@tanstack/react-query';
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
import { actingOn, actionNotice, resendSummary, type Notice } from './model';
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
}

/**
 * Owner actions for one task. The server decides what is allowed (`view.actions`) and checks
 * every submission; this panel only sends what the owner saw. A request whose outcome is unknown
 * stays as a resend with its original version, attempt and target until the owner resends or
 * discards it.
 */
export function TaskActions({ view, tracker, profiles, profilesError, offline }: TaskActionsProps) {
  const queryClient = useQueryClient();
  const taskId = view.task.id;
  const [notice, setNotice] = useState<Notice | null>(null);
  const [remembered, setRemembered] = useState(() => rememberedFrom(tracker, taskId));
  const [sending, setSending] = useState<TaskActionKind | null>(null);
  const [target, setTarget] = useState('');
  // The task as the owner saw it when the cancel dialog opened. The request is built from this, not
  // from the live view, so a change that lands while the dialog is open cannot retarget it.
  const [cancelSeen, setCancelSeen] = useState<TaskView | null>(null);

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
  // The dialog closes, sending nothing, once the task has moved on from what the owner confirmed:
  // a new version or attempt, or cancel no longer offered. The owner then re-opens it on the new task.
  const cancelMoved =
    cancelSeen !== null &&
    (cancelSeen.task.version !== view.task.version ||
      currentAttempt(cancelSeen)?.id !== currentAttempt(view)?.id ||
      !actionAvailability(view, 'cancel', { busy: false }).enabled);
  const cancelOpen = cancelSeen !== null && !cancelMoved;
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

  return (
    <section
      aria-labelledby="actions-heading"
      className="gap-sm p-md border-border flex flex-col border-t"
    >
      <h2 id="actions-heading" className="text-muted-foreground text-xs tracking-wide uppercase">
        [ACTIONS]
      </h2>
      <p className="text-muted-foreground text-xs">
        {actingOn(view)} If the task changes first, the server refuses and this page refreshes.
      </p>

      {blockers.length > 0 ? (
        <div data-testid="task-blockers" className="gap-xs flex flex-col text-sm">
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
            <p className="text-sm break-words">{resendSummary(intent)}</p>
            <p className="text-muted-foreground text-xs">
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
            {offline ? <p className="text-muted-foreground text-xs">Mainloop is offline.</p> : null}
            {sending === kind ? (
              <p role="status" className="text-muted-foreground text-xs">
                Sending request…
              </p>
            ) : null}
          </section>
        ) : null;
      })}

      <div className="gap-md flex flex-col">
        <div className="gap-xs flex flex-col">
          <Button
            type="button"
            variant="outline"
            data-testid="task-action-retry"
            disabled={!retry.enabled}
            onClick={() => void submit('retry')}
            className="self-start"
          >
            {LABELS.retry}
          </Button>
          {retry.reason ? <p className="text-muted-foreground text-xs">{retry.reason}</p> : null}
        </div>

        <div className="gap-xs flex flex-col">
          <label htmlFor="reassign-target" className="text-sm font-medium">
            Provider to reassign to
          </label>
          <select
            id="reassign-target"
            value={selected}
            disabled={targets.length === 0 || sending !== null || !reassignGate.enabled}
            onChange={(event) => setTarget(event.target.value)}
            className="border-border text-foreground bg-background px-md min-h-11 rounded-none border text-sm"
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
            className="self-start"
          >
            {LABELS.reassign}
          </Button>
          {reassign.reason ? (
            <p className="text-muted-foreground text-xs">{reassign.reason}</p>
          ) : null}
        </div>

        <div className="gap-xs flex flex-col">
          <Button
            type="button"
            variant="destructive"
            data-testid="task-action-cancel"
            disabled={!cancel.enabled}
            onClick={() => setCancelSeen(view)}
            className="self-start"
          >
            {LABELS.cancel}
          </Button>
          {cancel.reason ? <p className="text-muted-foreground text-xs">{cancel.reason}</p> : null}
          {cancelMoved && sending === null ? (
            <p role="status" className="text-muted-foreground text-xs">
              The task changed before you confirmed, so nothing was cancelled. Check it, then cancel
              again.
            </p>
          ) : null}
        </div>
      </div>

      {notice ? (
        <div
          role={notice.tone === 'info' ? 'status' : 'alert'}
          data-testid="task-action-notice"
          className={`gap-xs flex flex-col text-sm ${NOTICE_CLASS[notice.tone]}`}
        >
          <p className="break-words">{notice.text}</p>
          {notice.reason ? (
            <p className="text-xs">
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
