import type { ReactNode } from 'react';
import { HITLCard } from '@/features/hitl';
import { PanelBoundary } from '@/shell/PanelBoundary';
import { useConnection } from '@/shell/ConnectionBanner';
import { mainInputLock } from './mainProvider';
import { MainThreadHeader } from './MainThreadHeader';
import { SessionRequests } from './SessionRequests';
import { ThreadBody } from './ThreadBody';
import { useMainProvider } from './useMainProvider';
import { MAIN_THREAD, useThread } from './useThread';

export interface ChatPageProps {
  /** The approval request named by `?hitl=`, shown at the end of the thread with the other requests. */
  hitlId?: string;
  /** The native identity strip for the main session, supplied by the sessions slice. */
  renderIdentity?: (sessionId: string) => ReactNode;
  /** Text to put in the composer once, unsent. */
  prefill?: string;
  /** Called once the composer has taken `prefill`, so the caller can clear it. */
  onPrefillUsed?: () => void;
  /** Suggestions shown while the thread is empty. */
  starters?: ReactNode;
}

/** The main thread: header, conversation (ending with any pending requests) and the composer. */
export function ChatPage({
  hitlId,
  renderIdentity,
  prefill,
  onPrefillUsed,
  starters
}: ChatPageProps) {
  const chat = useThread(MAIN_THREAD);
  const offline = useConnection().status === 'offline';
  const { info } = chat;
  // A send, a stop or an unread thread holds the provider selection as well as offline does.
  const provider = useMainProvider(
    info,
    offline || chat.sendPending || chat.stopping || !chat.loaded
  );
  const inputLock = mainInputLock(info, provider);

  return (
    <div className="flex h-full min-h-0 flex-col" data-testid="main-thread">
      {info ? (
        <MainThreadHeader
          info={info}
          offline={offline}
          onStop={chat.stop}
          stopping={chat.stopping}
          renderIdentity={renderIdentity}
          provider={provider}
        />
      ) : null}

      <ThreadBody
        thread={chat}
        offline={offline}
        label="main thread"
        closedReason={inputLock}
        closedPlaceholder="Main input is locked"
        prefill={prefill}
        onPrefillUsed={onPrefillUsed}
        starters={starters}
        allowImages
        requests={
          <PanelBoundary name="Requests">
            {hitlId ? (
              <div className="border-border mx-md border">
                <HITLCard requestId={hitlId} />
              </div>
            ) : null}
            {info?.session_id ? (
              <SessionRequests sessionId={info.session_id} skip={hitlId} />
            ) : null}
          </PanelBoundary>
        }
      />
    </div>
  );
}
