import { ArrowDown } from 'lucide-react';
import { useCallback, useEffect, useLayoutEffect, useRef, useState, type ReactNode } from 'react';
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
/** A request card brought into view keeps this much of the thread above its top. */
const CARD_GAP_PX = 8;
/** Input inside a request card: growth after it is the owner's doing, not the card filling in. */
const CARD_INPUT = ['pointerdown', 'keydown'] as const;

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
  /** Requests waiting on the owner, shown after the newest message, inside the one scroll. */
  requests?: ReactNode;
  /** Shown in the empty state of a loaded thread, such as suggestions to start with. */
  starters?: ReactNode;
  retryDisabled: boolean;
  onRetry: (message: Message) => void;
}

/**
 * The thread. It is the only scrolling region, so the composer never scrolls away. Pending requests
 * are part of the thread rather than a pane of their own, so the centre never splits in two. They sit
 * after the log, not in it, so a screen reader does not announce a card's every change.
 *
 * While the owner is at the bottom, new messages keep the view there. A request card filling in is
 * brought into view top first, so a tall card is read from its heading. Once the owner works inside a
 * card, its growth (Show all, Request data) moves nothing until the next message.
 */
export function MessageList({
  messages,
  notices,
  assignments,
  agentKind,
  context,
  loaded,
  retryDisabled,
  onRetry,
  live,
  requests,
  starters
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

  const log = useRef<HTMLDivElement>(null);
  const pending = useRef<HTMLDivElement>(null);
  // The owner has worked inside a request card since the last message.
  const handled = useRef(false);
  const hasRequests = requests !== undefined;

  const last = messages.at(-1);
  // A delivery notice can land on the last message without changing the list, so it re-pins too.
  useLayoutEffect(() => {
    const el = scroller.current;
    if (el && stuck.current) el.scrollTop = el.scrollHeight;
  }, [messages.length, last?.content, notices, live?.seq]);
  useEffect(() => {
    handled.current = false;
  }, [messages.length]);

  // Messages and cards fill in after they mount without the list changing, so growth re-pins too: the
  // log's to the bottom, a card's to its top, unless the owner made it grow.
  useEffect(() => {
    const box = scroller.current;
    if (!box || typeof ResizeObserver === 'undefined') return;
    const observer = new ResizeObserver((entries) => {
      if (!stuck.current) return;
      const cards = pending.current;
      if (cards && entries.some((entry) => entry.target === cards)) {
        if (handled.current) return;
        const offset = cards.getBoundingClientRect().top - box.getBoundingClientRect().top;
        box.scrollTop = Math.min(box.scrollHeight, box.scrollTop + offset - CARD_GAP_PX);
        // Settled now, not on the scroll event: the log's growth in the same frame must not pull the
        // view back to the bottom.
        stuck.current = distanceFromBottom() <= STICK_PX;
        return;
      }
      box.scrollTop = box.scrollHeight;
    });
    for (const el of [log.current, pending.current]) if (el) observer.observe(el);
    return () => observer.disconnect();
  }, [hasRequests]);

  useEffect(() => {
    const cards = pending.current;
    if (!cards) return;
    const mark = () => {
      handled.current = true;
    };
    for (const type of CARD_INPUT) cards.addEventListener(type, mark, true);
    return () => {
      for (const type of CARD_INPUT) cards.removeEventListener(type, mark, true);
    };
  }, [hasRequests]);

  return (
    <div className="relative min-h-0 flex-1">
      <div
        ref={scroller}
        onScroll={onScroll}
        className="h-full overflow-y-auto"
        data-testid="message-list"
      >
        <div className="gap-sm py-md flex min-h-full flex-col">
          <div
            ref={log}
            role="log"
            aria-label="Conversation"
            className="gap-sm flex flex-1 flex-col"
          >
            {messages.length === 0 && !live ? (
              <div className="gap-md m-auto flex flex-col items-center text-center text-sm">
                <div className="text-muted-foreground">
                  <p className="text-term-accent">
                    {loaded ? '$ mainloop --help' : '$ connecting'}
                  </p>
                  <p>{loaded ? 'Start a conversation to begin' : 'Waiting for the backend…'}</p>
                </div>
                {loaded ? starters : null}
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
          {hasRequests ? (
            <div ref={pending} data-testid="thread-requests">
              {requests}
            </div>
          ) : null}
        </div>
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
