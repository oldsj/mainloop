import type { ReactNode } from 'react';
import { ErrorRecovery } from '@/components/shared/error-recovery';
import { CONVERSATION } from '@/components/shared/measure';
import { cn } from '@/components/ui/cn';
import { Composer } from './Composer';
import { assignmentIds } from './assignments';
import { MessageList } from './MessageList';
import { fieldText, useRunDefaults, type DefaultsRole } from './runDefaults';
import type { SlashCommandSpec } from './slashCommand';
import { TurnRecovery } from './TurnRecovery';
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
  /** The composer accepts a pasted or dropped image. Only the main thread sets this. */
  allowImages?: boolean;
  /**
   * Intercepts a composer send before the thread's own `send`, for a local command such as
   * `/model` that must not become a chat message. Returning null defers to the thread's `send`.
   */
  onSend?: (text: string) => Promise<boolean> | null;
  /** The last local command's output, shown above the composer until the next send. */
  commandNotice?: string | null;
  /** Slash commands the composer's autosuggest dropdown offers. Omitted where there are none. */
  commands?: readonly SlashCommandSpec[];
  /** The session controls chip, shown with the composer. Only the main thread has one. */
  controls?: ReactNode;
  /**
   * Which of the owner's session defaults this thread falls under: the main thread's, or a
   * worker's (every other session). The composer's model and effort line names it.
   */
  defaultsRole?: DefaultsRole;
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
  starters,
  allowImages = false,
  onSend,
  commandNotice = null,
  commands,
  controls,
  defaultsRole = 'worker'
}: ThreadBodyProps) {
  const live = useLiveTurn(thread.sessionId, thread.busy, thread.messages);
  // The line says whose choice each value is: the session's own, the owner's default for its role,
  // or the provider's. Until the owner's defaults are read it shows the session's own choice only.
  const run = useRunDefaults(defaultsRole, thread.native);
  const modelEffort = !thread.native
    ? null
    : run
      ? {
          model: fieldText(run.model, defaultsRole),
          effort: fieldText(run.effort, defaultsRole)
        }
      : {
          model: thread.native.model_override ?? null,
          effort: thread.native.effort_override ?? null
        };
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
        <p
          className={cn('text-muted-foreground px-md py-xs text-caption', CONVERSATION)}
          role="status"
        >
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
          className={cn(
            'text-destructive px-md py-xs text-body shrink-0 [overflow-wrap:anywhere]',
            CONVERSATION
          )}
          role="alert"
          data-testid="send-error"
        >
          {thread.sendError}
        </p>
      ) : null}
      {thread.stopError ? (
        <p
          className={cn('text-destructive px-md py-xs text-body shrink-0', CONVERSATION)}
          role="alert"
        >
          {thread.stopError}
        </p>
      ) : null}
      <div className={cn('shrink-0', CONVERSATION)}>
        <TurnRecovery thread={thread} offline={offline} />
      </div>

      {closedReason ? (
        <p
          className={cn('text-muted-foreground px-md py-xs text-body shrink-0', CONVERSATION)}
          role="status"
        >
          {closedReason}
        </p>
      ) : null}
      {commandNotice ? (
        <p
          className={cn(
            'text-muted-foreground px-md py-xs text-body shrink-0 whitespace-pre-line',
            CONVERSATION
          )}
          role="status"
          data-testid="command-notice"
        >
          {commandNotice}
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
        onSend={(text, images) =>
          onSend?.(text) ?? thread.send(text, thread.conversationId, images)
        }
        allowImages={allowImages}
        commands={commands}
        modelEffort={modelEffort}
        controls={controls}
      />
    </>
  );
}
