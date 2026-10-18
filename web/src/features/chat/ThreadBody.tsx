import type { ReactNode } from 'react';
import { ErrorRecovery } from '@/components/shared/error-recovery';
import { Composer } from './Composer';
import { assignmentIds } from './assignments';
import { MessageList } from './MessageList';
import { useLiveTurn } from './useLiveTurn';
import type { ThreadView } from './useThread';

export interface ThreadBodyProps {
  thread: ThreadView;
  /** The backend is unreachable, so sends and retries are held. */
  offline: boolean;
  /** Names the thread in the load error, as in "Could not load the main thread". */
  label: string;
  /** Why the thread takes no more messages. Set, the box is locked and this line says why. */
  closedReason?: string | null;
  /** The locked box's placeholder, when the thread's lock is not "closed to new messages". */
  closedPlaceholder?: string;
  /** Text to put in the composer once, unsent. */
  prefill?: string;
  /** Called once the composer has taken `prefill`. */
  onPrefillUsed?: () => void;
  /** Requests waiting on the owner, shown at the end of the conversation. */
  requests?: ReactNode;
  /** Shown in the empty state of a loaded thread. */
  starters?: ReactNode;
}

/** The conversation timeline, the send and stop errors, and the composer, for any thread. */
export function ThreadBody({
  thread,
  offline,
  label,
  closedReason = null,
  closedPlaceholder,
  prefill,
  onPrefillUsed,
  requests,
  starters
}: ThreadBodyProps) {
  const live = useLiveTurn(thread.sessionId, thread.busy, thread.messages);
  return (
    <>
      {thread.loadFailed && !offline ? (
        <div className="p-md">
          <ErrorRecovery
            title={`Could not load the ${label}`}
            message="The thread did not load. Your drafts are kept."
            onRetry={thread.reload}
          />
        </div>
      ) : null}
      {thread.conversationFailed && !thread.loaded ? (
        <div className="p-md">
          <ErrorRecovery
            title="Could not load the conversation"
            message="The messages did not load. Your drafts are kept."
            onRetry={thread.reloadConversation}
          />
        </div>
      ) : null}
      {thread.conversationFailed && thread.loaded ? (
        <p className="text-muted-foreground px-md py-xs text-xs" role="status">
          Could not refresh the conversation. Showing the last messages.
        </p>
      ) : null}

      <MessageList
        messages={thread.messages}
        notices={thread.notices}
        assignments={assignmentIds(thread.messages)}
        agentKind={thread.native?.kind as 'claude' | 'codex' | undefined}
        context={thread.context}
        loaded={thread.loaded}
        live={live}
        requests={requests}
        starters={starters}
        retryDisabled={thread.busy || thread.sendPending || offline}
        onRetry={(message) => void thread.retry(message)}
      />

      {thread.sendError ? (
        <p
          className="text-destructive px-md py-xs shrink-0 text-sm [overflow-wrap:anywhere]"
          role="alert"
          data-testid="send-error"
        >
          {thread.sendError}
        </p>
      ) : null}
      {thread.stopError ? (
        <p className="text-destructive px-md py-xs shrink-0 text-sm" role="alert">
          {thread.stopError}
        </p>
      ) : null}

      {closedReason ? (
        <p className="text-muted-foreground px-md py-xs shrink-0 text-sm" role="status">
          {closedReason}
        </p>
      ) : null}
      <Composer
        draftKey={thread.draftKey}
        busy={thread.sendPending}
        turnBusy={thread.busy}
        loading={!thread.loaded && !thread.conversationFailed && !thread.loadFailed}
        unavailable={thread.loadFailed}
        offline={offline}
        closed={closedReason !== null}
        closedPlaceholder={closedPlaceholder}
        prefill={prefill}
        onPrefillUsed={onPrefillUsed}
        onSend={(text) => thread.send(text, thread.conversationId)}
      />
    </>
  );
}
