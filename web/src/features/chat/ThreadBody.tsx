import { ErrorRecovery } from '@/components/shared/error-recovery';
import { Composer } from './Composer';
import { MessageList } from './MessageList';
import type { ThreadView } from './useThread';

export interface ThreadBodyProps {
  thread: ThreadView;
  /** The backend is unreachable, so sends and retries are held. */
  offline: boolean;
  /** Names the thread in the load error, as in "Could not load the main thread". */
  label: string;
}

/** The conversation timeline, the send and stop errors, and the composer, for any thread. */
export function ThreadBody({ thread, offline, label }: ThreadBodyProps) {
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
        agentKind={thread.native?.kind as 'claude' | 'codex' | undefined}
        loaded={thread.loaded}
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

      <Composer
        draftKey={thread.draftKey}
        busy={thread.busy || thread.sendPending}
        loading={!thread.threadKnown && !thread.loadFailed}
        unavailable={thread.loadFailed}
        offline={offline}
        onSend={(text) => thread.send(text, thread.conversationId)}
      />
    </>
  );
}
