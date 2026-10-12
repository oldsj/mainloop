import { ArrowDown } from 'lucide-react';
import { useCallback, useLayoutEffect, useRef, useState } from 'react';
import { Button } from '@/components/ui/button';
import type { Message } from '@/lib/contract';
import type { DeliveryNotice } from '@/lib/delivery';
import type { ProgressFrame } from '@/api';
import { MessageBubble } from './MessageBubble';
import { LiveTurn } from './LiveTurn';

/** Within this distance of the end, new messages keep the view pinned to the bottom. */
export const STICK_PX = 150;
/** Further than this from the end, the scroll-to-bottom button appears. */
export const SHOW_BUTTON_PX = 100;

export interface MessageListProps {
  messages: Message[];
  notices: Map<string, DeliveryNotice>;
  /** Message ids Mainloop wrote on the owner's behalf; they show as collapsed assignments. */
  assignments?: ReadonlySet<string>;
  agentKind?: 'claude' | 'codex' | null;
  /** Where the messages were written, shown in each prompt label. */
  context?: string;
  /** The thread has been read at least once; before that an empty list means "not loaded". */
  loaded: boolean;
  /** The turn still running, shown below the last message. */
  live?: ProgressFrame;
  retryDisabled: boolean;
  onRetry: (message: Message) => void;
}

/** The thread. It is the only scrolling region, so the composer never scrolls away. */
export function MessageList({
  messages,
  notices,
  assignments,
  agentKind,
  context,
  loaded,
  retryDisabled,
  onRetry,
  live
}: MessageListProps) {
  const scroller = useRef<HTMLDivElement>(null);
  // True on the first render, so the first load lands on the newest message.
  const stuck = useRef(true);
  const [showButton, setShowButton] = useState(false);

  const distanceFromBottom = () => {
    const el = scroller.current;
    return el ? el.scrollHeight - el.scrollTop - el.clientHeight : 0;
  };

  const onScroll = () => {
    const distance = distanceFromBottom();
    stuck.current = distance <= STICK_PX;
    setShowButton(distance > SHOW_BUTTON_PX);
  };

  const scrollToBottom = useCallback(() => {
    const el = scroller.current;
    if (!el) return;
    stuck.current = true;
    setShowButton(false);
    el.scrollTo?.({ top: el.scrollHeight, behavior: 'smooth' });
  }, []);

  const last = messages.at(-1);
  // A delivery notice can land on the last message without changing the list, so it re-pins too.
  useLayoutEffect(() => {
    const el = scroller.current;
    if (el && stuck.current) el.scrollTop = el.scrollHeight;
  }, [messages.length, last?.content, notices, live?.seq]);

  return (
    <div className="relative min-h-0 flex-1">
      <div
        ref={scroller}
        onScroll={onScroll}
        role="log"
        aria-label="Conversation"
        className="gap-sm py-md flex h-full flex-col overflow-y-auto"
        data-testid="message-list"
      >
        {messages.length === 0 && !live ? (
          <div className="text-muted-foreground m-auto text-center text-sm">
            <p className="text-term-accent">{loaded ? '$ mainloop --help' : '$ connecting'}</p>
            <p>{loaded ? 'Start a conversation to begin' : 'Waiting for the backend…'}</p>
          </div>
        ) : (
          messages.map((message) => (
            <MessageBubble
              key={message.id}
              message={message}
              agentKind={agentKind}
              context={context}
              notice={notices.get(message.id)}
              assignment={assignments?.has(message.id)}
              retryDisabled={retryDisabled}
              onRetry={onRetry}
            />
          ))
        )}
        {live ? <LiveTurn frame={live} context={context} agentKind={agentKind} /> : null}
      </div>
      {showButton ? (
        <Button
          variant="outline"
          size="icon"
          className="bg-background absolute right-4 bottom-4"
          aria-label="Scroll to the newest message"
          onClick={scrollToBottom}
          data-testid="scroll-to-bottom"
        >
          <ArrowDown aria-hidden="true" />
        </Button>
      ) : null}
    </div>
  );
}
