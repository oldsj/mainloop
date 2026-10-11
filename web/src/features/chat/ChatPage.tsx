import type { ReactNode } from 'react';
import { HITLCard } from '@/features/hitl';
import { ErrorRecovery } from '@/components/shared/error-recovery';
import { PanelBoundary } from '@/shell/PanelBoundary';
import { useConnection } from '@/shell/ConnectionBanner';
import { Composer } from './Composer';
import { NEW_CONVERSATION } from './drafts';
import { MainThreadHeader } from './MainThreadHeader';
import { MessageList } from './MessageList';
import { SessionRequests } from './SessionRequests';
import { useMainThread } from './useMainThread';

export interface ChatPageProps {
  /** The approval request named by `?hitl=`, shown at the top of the thread. */
  hitlId?: string;
  /** The native identity strip for the main session, supplied by the sessions slice. */
  renderIdentity?: (sessionId: string) => ReactNode;
}

/** The main thread: header, conversation, pending requests and the composer. */
export function ChatPage({ hitlId, renderIdentity }: ChatPageProps) {
  const chat = useMainThread();
  const offline = useConnection().status === 'offline';
  const { info } = chat;

  return (
    <div className="flex h-full min-h-0 flex-col" data-testid="main-thread">
      {info ? (
        <MainThreadHeader
          info={info}
          offline={offline}
          onStop={chat.stop}
          stopping={chat.stopping}
          renderIdentity={renderIdentity}
        />
      ) : null}

      <PanelBoundary name="Requests">
        {hitlId ? (
          <div className="border-border max-h-[50vh] shrink-0 overflow-y-auto border-b">
            <HITLCard requestId={hitlId} />
          </div>
        ) : null}
        {info?.session_id ? <SessionRequests sessionId={info.session_id} skip={hitlId} /> : null}
      </PanelBoundary>

      {chat.loadFailed && !offline ? (
        <div className="p-md">
          <ErrorRecovery
            title="Could not load the main thread"
            message="The thread did not load. Your drafts are kept."
            onRetry={chat.reload}
          />
        </div>
      ) : null}
      {chat.conversationFailed && !chat.loaded ? (
        <div className="p-md">
          <ErrorRecovery
            title="Could not load the conversation"
            message="The messages did not load. Your drafts are kept."
            onRetry={chat.reloadConversation}
          />
        </div>
      ) : null}
      {chat.conversationFailed && chat.loaded ? (
        <p className="text-muted-foreground px-md py-xs text-xs" role="status">
          Could not refresh the conversation. Showing the last messages.
        </p>
      ) : null}

      <MessageList
        messages={chat.messages}
        notices={chat.notices}
        agentKind={info?.native?.kind as 'claude' | 'codex' | undefined}
        loaded={chat.loaded}
        retryDisabled={chat.busy || chat.sendPending || offline}
        onRetry={(message) => void chat.retry(message)}
      />

      {chat.sendError ? (
        <p
          className="text-destructive px-md py-xs shrink-0 text-sm [overflow-wrap:anywhere]"
          role="alert"
          data-testid="send-error"
        >
          {chat.sendError}
        </p>
      ) : null}
      {chat.stopError ? (
        <p className="text-destructive px-md py-xs shrink-0 text-sm" role="alert">
          {chat.stopError}
        </p>
      ) : null}

      <Composer
        draftKey={chat.conversationId ?? NEW_CONVERSATION}
        busy={chat.busy || chat.sendPending}
        loading={!chat.threadKnown && !chat.loadFailed}
        unavailable={chat.loadFailed}
        offline={offline}
        onSend={(text) => chat.send(text, chat.conversationId)}
      />
    </div>
  );
}
