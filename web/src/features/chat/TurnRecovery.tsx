import { useState } from 'react';
import { ConfirmDialog } from '@/components/shared/confirm-dialog';
import type { ThreadView } from './useThread';

export interface TurnRecoveryProps {
  thread: ThreadView;
  offline: boolean;
  /** The clock, for the quiet time of a stalled turn. */
  now?: () => number;
}

/** Whole minutes since `iso`, at least one. */
export function quietMinutes(iso: string, now: number): number {
  const since = Date.parse(iso);
  if (Number.isNaN(since)) return 1;
  return Math.max(1, Math.floor((now - since) / 60_000));
}

const BUTTON =
  'border-border text-foreground hover:text-term-red px-md min-h-11 border disabled:opacity-50';

/**
 * What the owner can do about a turn that is stuck. A stalled turn (the server saw no task activity
 * past its allowance) says how long it has been quiet and offers stop; it may still finish, and the
 * next event clears it. When the thread can be restarted, a stalled turn or one whose stop failed
 * also offers restart. Restarting asks first, in the app, because the native context is lost: the
 * open turn ends as failed and is not resent, and queued messages run on a new native session that
 * carries the recent conversation. Nothing here retries or replays anything on its own.
 */
export function TurnRecovery({ thread, offline, now = Date.now }: TurnRecoveryProps) {
  const [confirm, setConfirm] = useState(false);
  const stall = thread.native?.stalled_turn ?? null;
  const canRestart = thread.restartable;
  if (!thread.busy) return null;
  let why: string;
  if (stall) {
    const minutes = quietMinutes(stall.last_activity_at, now());
    why = `No activity for ${minutes}m${stall.tool_running ? ' while a tool runs' : ''}. It may still finish.`;
  } else if (thread.stopError && canRestart) {
    why = 'The turn could not be stopped.';
  } else {
    return null;
  }
  const ask = canRestart ? (stall ? 'Stop or restart?' : 'Restart?') : 'Stop it?';
  return (
    <div
      className="border-border mx-md my-xs gap-sm p-sm flex shrink-0 flex-wrap items-center border text-sm"
      data-testid="turn-recovery"
    >
      <p className="text-foreground min-w-0 flex-1 [overflow-wrap:anywhere]" role="status">
        {why} {ask}
        {canRestart
          ? ' Restarting the native session ends this turn as failed; your queued messages then run on a fresh session.'
          : ''}
      </p>
      {stall && !thread.stopError ? (
        <button
          type="button"
          className={BUTTON}
          disabled={thread.stopping || offline}
          onClick={thread.stop}
          data-testid="turn-recovery-stop"
        >
          {thread.stopping ? 'stopping…' : 'stop'}
        </button>
      ) : null}
      {canRestart ? (
        <button
          type="button"
          className={BUTTON}
          disabled={thread.restarting || offline}
          onClick={() => setConfirm(true)}
          data-testid="turn-restart"
        >
          {thread.restarting ? 'restarting…' : 'restart session'}
        </button>
      ) : null}
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
