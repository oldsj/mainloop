import { useState } from 'react';
import { ConfirmDialog } from '@/components/shared/confirm-dialog';
import type { ThreadView } from './useThread';

export interface TurnRecoveryProps {
  thread: ThreadView;
  offline: boolean;
  /** Why recovery is offered, shown first. Null hides the panel unless a stop just failed. */
  reason?: string | null;
}

/**
 * The way out of a turn that cannot be stopped: restart the thread's native session. Shown while a
 * turn is in flight and either the last stop failed or `reason` says why (a stalled turn). Restarting
 * asks first, in the app, because the native context is lost: the open turn ends as failed and is
 * not resent, and queued messages run on a new native session that carries the recent conversation.
 */
export function TurnRecovery({ thread, offline, reason = null }: TurnRecoveryProps) {
  const [confirm, setConfirm] = useState(false);
  const why = reason ?? (thread.stopError ? 'The turn could not be stopped.' : null);
  if (!thread.restartable || !thread.busy || why === null) return null;
  return (
    <div
      className="border-border mx-md my-xs gap-sm p-sm flex shrink-0 flex-wrap items-center border text-sm"
      data-testid="turn-recovery"
    >
      <p className="text-foreground min-w-0 flex-1 [overflow-wrap:anywhere]" role="status">
        {why} Restarting the native session ends this turn as failed; your queued messages then run
        on a fresh session.
      </p>
      <button
        type="button"
        className="border-border text-foreground hover:text-term-red px-md min-h-11 border disabled:opacity-50"
        disabled={thread.restarting || offline}
        onClick={() => setConfirm(true)}
        data-testid="turn-restart"
      >
        {thread.restarting ? 'restarting…' : 'restart session'}
      </button>
      {thread.restartError ? (
        <p className="text-destructive w-full [overflow-wrap:anywhere]" role="alert">
          {thread.restartError}
        </p>
      ) : null}
      <ConfirmDialog
        open={confirm}
        onOpenChange={setConfirm}
        title="Restart the native session?"
        description="The native context is lost: tool state and anything the agent knew that is not in this conversation. The open turn ends as failed and is not sent again. The next message starts a new native session that carries the recent conversation."
        confirmLabel="Restart"
        cancelLabel="Keep waiting"
        destructive
        pending={thread.restarting}
        onConfirm={() => {
          thread.restart();
          setConfirm(false);
        }}
      />
    </div>
  );
}
